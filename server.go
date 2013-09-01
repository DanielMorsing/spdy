// Copyright (c) 2013, Daniel Morsing
// For more information, see the LICENSE file

// package spdy implements a HTTP server on top of code.google.com/p/go.net/spdy.
//
// Note that SPDY is normally deployed on the HTTPS port and the protocol to use is then
// negotiated over TLS. To enable fallback to HTTPS when the client is not SPDY enabled, this package
// starts a HTTPS server that connections are forwarded to if they fail negotiation.
//
// The HTTPS fallback feature can be disabled by providing a TLS config which does not include
// http/1.1 in its valid NPN protocols.
//
// TODO: Server Push, Client Certificate validation
package spdy

import (
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"time"
)

// Server defines the parameters for running a SPDY server.
type Server struct {
	Addr    string       // TCP address to listen on. ":https" if empty.
	Handler http.Handler // handler to invoke. Unlike "net/http" this must be non-nil.

	ReadTimeout  time.Duration // Maximum duration before timing out on reads.
	WriteTimeout time.Duration // Maximum duration before timing out on writes.

	// Optional TLS config to be used on this connection.
	// To disable HTTPS fallback, set the NextProtos field to a value that includes "spdy/3"
	TLSConfig *tls.Config
}

// ListenAndServeTLS listens on srv.Addr and calls Serve to handle incoming connections.
//
// certFile and keyFile must be filenames to a pair of valid certificate and key.
func (srv *Server) ListenAndServeTLS(certFile, keyFile string) error {
	config := &tls.Config{}
	if srv.TLSConfig == nil {
		srv.TLSConfig = config
	} else {
		config = srv.TLSConfig
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}
	config.Certificates = []tls.Certificate{cert}

	spdyAvail, httpAvail := srv.validateNPN(config)
	if !spdyAvail {
		return errors.New("Started SPDY server as exclusively https")
	}

	addr := srv.addr()
	l, err := srv.negotiateListen(addr, httpAvail)
	if err != nil {
		return err
	}
	return srv.Serve(l)

}

// validateNPN will validate NPN and return whether the protocols are available.
// If none are set, both http/1.1 and spdy/3 will be made available.
func (srv *Server) validateNPN(config *tls.Config) (spdy, http bool) {
	np := config.NextProtos
	if np == nil {
		config.NextProtos = []string{"spdy/3", "http/1.1"}
		spdy = true
		http = true
		return
	}

	for _, v := range np {
		switch v {
		case "spdy/3":
			spdy = true
		case "http/1.1":
			http = true
		}
	}
	return
}

// listenAndServe listens and serve on the address given.
// Since SPDY relies on TLS with protocol negotiation,
// this method should only be used for local testing.
func (srv *Server) ListenAndServe() error {
	addr := srv.addr()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return srv.Serve(l)
}

// Serve serves connection off the provided listener.
// Note that it is the listeners responsibility to negotiate the
// protocol used.
func (srv *Server) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			log.Println("accept:", err)
			continue
		}

		sess, err := srv.newSession(c)
		if err != nil {
			log.Println("could not start session:", err)
			c.Close()
			continue
		}
		go sess.serve()
	}
}

func (srv *Server) addr() string {
	if srv.Addr != "" {
		return srv.Addr
	}
	return ":https"
}

// create a listener that negotiates NPN. If http is available, forward to a http server.
func (srv *Server) negotiateListen(addr string, httpAvail bool) (net.Listener, error) {
	l, err := tls.Listen("tcp", addr, srv.TLSConfig)
	if err != nil {
		return nil, err
	}
	ngl := &negotiateListen{l, nil}
	if httpAvail {
		ch, fwl := newForwardlisten(ngl)
		ngl.httpch = ch
		go srv.startHttpfallback(fwl)
	}
	return ngl, nil
}

// negotiateListen is a listener that negotiates spdy connections.
// if http is enabled, it will forward the connection to the fallback server
type negotiateListen struct {
	net.Listener
	httpch chan net.Conn
}

func (nl *negotiateListen) Accept() (net.Conn, error) {
	for {
		c, err := nl.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ctls := c.(*tls.Conn)
		err = ctls.Handshake()
		if err != nil {
			log.Println("handshake:", err)
			ctls.Close()
			continue
		}
		cstate := ctls.ConnectionState()
		switch cstate.NegotiatedProtocol {
		case "spdy/3":
			return c, nil
		case "http/1.1", "":
			if nl.httpch != nil {
				nl.httpch <- c
				continue
			}
			fallthrough
		default:
			// unsupported protocol
			c.Close()
		}
	}
}

// forwardlisten provides a listener interface for the HTTPS server.
type forwardlisten struct {
	ch chan net.Conn
	l  net.Listener
}

func (f *forwardlisten) Accept() (net.Conn, error) {
	c, ok := <-f.ch
	if !ok {
		return nil, io.EOF
	}
	return c, nil
}

func (f *forwardlisten) Close() error {
	close(f.ch)
	return nil
}

func (f *forwardlisten) Addr() net.Addr {
	return f.l.Addr()
}

func newForwardlisten(l net.Listener) (chan net.Conn, *forwardlisten) {
	ch := make(chan net.Conn)
	return ch, &forwardlisten{
		ch: ch,
		l:  l,
	}
}

func (srv *Server) startHttpfallback(l net.Listener) error {
	httpServer := http.Server{
		Handler:      srv.Handler,
		ReadTimeout:  srv.ReadTimeout,
		WriteTimeout: srv.WriteTimeout,
		TLSConfig:    srv.TLSConfig,
	}
	return httpServer.Serve(l)
}
