// Copyright (c) 2013, Daniel Morsing
// For more information, see the LICENSE file

// package spdy implements a HTTP server on top of code.google.com/p/go.net/spdy.
//
// This package provides a http server that uses the TLSNextProto feature in net/http to serve connections that where negotiated to be SPDY 3.
// The HTTPS fallback feature can be disabled by providing a TLS config which does not include
// http/1.1 in its valid NPN protocols.
//
// TODO: Server Push, Client Certificate validation
package spdy

import (
	"crypto/tls"
	"log"
	"net/http"
)

// Server defines the parameters for running a SPDY server.
type Server struct {
	http.Server
}

// ListenAndServeTLS starts a SPDY forwarding server, using the parameters in the embedded
// http.Server.
//
// certFile and keyFile must be filenames to a pair of valid certificate and key.
func (srv *Server) ListenAndServeTLS(certFile, keyFile string) error {
	hs := &srv.Server
	config := &tls.Config{}
	if hs.TLSConfig == nil {
		hs.TLSConfig = config
	} else {
		config = hs.TLSConfig
	}

	srv.makeNPN(config)

	if hs.TLSNextProto == nil {
		hs.TLSNextProto = make(map[string]func(*http.Server, *tls.Conn, http.Handler))
	}

	if _, ok := hs.TLSNextProto["spdy/3"]; !ok {
		hs.TLSNextProto["spdy/3"] = srv.servespdy
	}

	return hs.ListenAndServeTLS(certFile, keyFile)

}

func (srv *Server) makeNPN(config *tls.Config) {
	np := config.NextProtos
	if np == nil {
		config.NextProtos = []string{"spdy/3", "http/1.1"}
	}
	return
}

func (srv *Server) servespdy(s *http.Server, conn *tls.Conn, hnd http.Handler) {
	sess, err := newSession(s, conn, hnd)
	if err != nil {
		log.Println(err)
		return
	}
	sess.serve()
}
