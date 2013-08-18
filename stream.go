// Copyright (c) 2013, Daniel Morsing
// For more information, see the LICENSE file

package spdy

import (
	"bufio"
	"code.google.com/p/go.net/spdy"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"sync"
)

// stream represents a spdy stream within a session
type stream struct {
	// the stream id for this stream
	id spdy.StreamId

	// window size
	window      uint32
	priority    uint8
	reset       chan error
	retch       chan rwResult
	readch      chan rwRead
	session     *session
	receivedFin bool
	sentFin     bool
	blocked     bool

	// mutex to protect the window variable
	mu sync.Mutex
}

func newStream(sess *session, syn *spdy.SynStreamFrame) *stream {
	s := &stream{
		id:       syn.StreamId,
		priority: syn.Priority,
		window:   sess.initialWindow,
		reset:    make(chan error),
		retch:    make(chan rwResult),
		readch:   make(chan rwRead),
		session:  sess,
	}
	if syn.CFHeader.Flags&spdy.ControlFlagFin != 0 {
		s.receivedFin = true
	}
	return s

}

func (str *stream) handleReq(hnd http.Handler, hdr http.Header) {
	req := mkrequest(hdr)
	rw := rwWriter{
		responseWriter: responseWriter{
			stream: str,
			header: make(http.Header),
			retch:  str.retch,
			ch:     str.session.streamch,
			readch: str.readch,
			ackch:  str.session.ackch,
			finch:  str.session.finch,
			reset:  str.reset,
		},
	}
	rw.bufw = bufio.NewWriter(&rw.responseWriter)
	if !str.receivedFin {
		req.Body = ioutil.NopCloser(&rw.responseWriter)
	}
	go func() {
		hnd.ServeHTTP(&rw, req)
		err := rw.close()
		if err != nil {
			log.Println("handlereq:", err)
		}
	}()
}

var requiredHeaders = [...]string{
	":method",
	":path",
	":version",
	":scheme",
	":host",
}

func mkrequest(hdr http.Header) *http.Request {

	for _, r := range requiredHeaders {
		if _, ok := hdr[r]; !ok {
			panic("STREAM ERROR")
		}
	}
	rm := make([]string, 0, len(hdr))
	req := new(http.Request)
	req.URL = new(url.URL)
	for k, v := range hdr {
		if k[0] == ':' {
			rm = append(rm, k)
		}
		switch k {
		case ":method":
			req.Method = v[0]
		case ":path":
			req.URL.Path = v[0]
		case ":version":
			s := v[0]
			req.URL.Host = s
			req.Host = s
			major, minor, ok := http.ParseHTTPVersion(s)
			if ok {
				req.ProtoMajor = major
				req.ProtoMinor = minor
			}
		case ":scheme":
			req.URL.Scheme = v[0]
		case ":host":
			req.Host = v[0]
			req.URL.Host = v[0]
		}
	}
	for _, k := range rm {
		delete(hdr, k)
	}
	req.Header = hdr
	return req
}

// builds a dataframe for the byte slice and updates the window.
// if the window is empty, set the blocked field and return 0
func (str *stream) buildDataframe(data *spdy.DataFrame, b []byte) int {
	data.StreamId = str.id
	data.Flags = 0
	str.mu.Lock()
	defer str.mu.Unlock()
	if str.window == 0 {
		str.blocked = true
		return 0
	}
	if len(b) > int(str.window) {
		b = b[:str.window]
	}
	str.window -= uint32(len(b))
	data.Data = b
	return len(b)
}

// small type to wrap a bufio.Writer around the responseWriter.
type rwWriter struct {
	bufw *bufio.Writer
	responseWriter
}

func (rw *rwWriter) Write(b []byte) (int, error) {
	if !rw.headerWritten {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.bufw.Write(b)
}

func (rw *rwWriter) close() error {
	rw.bufw.Flush()
	return rw.responseWriter.close()
}

type responseWriter struct {
	stream        *stream
	headerWritten bool
	header        http.Header

	// channels to send bytes to session and receive results
	ch    chan rwWrite
	retch chan rwResult
	// channel which we receive resets on
	reset chan error

	readch  chan rwRead
	left    []byte
	readerr error
	eof     bool

	// channels to send header
	ackch  chan rwAck
	finch  chan *stream
	closed bool
}

func (rw *responseWriter) isClosed() bool {
	if rw.closed {
		return true
	}
	select {
	case <-rw.reset:
		rw.closed = true
		return true
	default:
		return false
	}
}

func (rw *responseWriter) Header() http.Header {
	return rw.header
}

func (rw *responseWriter) WriteHeader(code int) {
	if rw.headerWritten {
		log.Println("Header written twice")
		return
	}
	s := fmt.Sprintf("%d %s", code, http.StatusText(code))
	rw.header.Set(":status", s)
	rw.header.Set(":version", "HTTP/1.1")
	rw.sendAck(rw.header)
	rw.headerWritten = true
}

// types for ferrying data back and forth to the session
type rwWrite struct {
	b   []byte
	str *stream
	ret chan rwResult
}

type rwResult struct {
	n   int
	err error
}

// Writes body to the stream
func (rw *responseWriter) Write(b []byte) (int, error) {
	n := 0
	var err error
	sw := rwWrite{b, rw.stream, rw.retch}
	ch := rw.ch
	retch := chan rwResult(nil)
	for {
		if rw.isClosed() {
			return 0, io.ErrClosedPipe
		}
		if len(b) == 0 {
			break
		}

		sw.b = b
		// Subtle code alert!
		// if we hit the flow control cap, we wont receive an error
		// until the session has had the window expanded.
		// Use the trick from advanced concurrency pattern talk
		// to handle resets.
		select {
		case ch <- sw:
			retch = rw.retch
			ch = nil
			continue
		case ret := <-retch:
			err = ret.err
			n += ret.n
			if err != nil {
				break
			}
			b = b[ret.n:]
			ch = rw.ch
			retch = nil
			continue
		case err = <-rw.reset:
			rw.closed = true
			return n, io.ErrClosedPipe
		}
	}
	return n, err
}

type rwRead struct {
	b   []byte
	err error
}

func (rw *responseWriter) Read(b []byte) (int, error) {
	// Subtle code alert.
	// if we get a bigger frame than the b passed,
	// put the remainder in left and defer the error until
	// we've read it all.
	if rw.eof || rw.isClosed() {
		return 0, io.EOF
	}
	if rw.left != nil {
		n := copy(b, rw.left)
		if n == len(rw.left) {
			rw.left = nil
			err := rw.readerr
			if err == io.EOF {
				rw.eof = true
			}
			return n, err
		}
		rw.left = rw.left[n:]
		return n, nil
	}
	select {
	case <-rw.reset:
		rw.closed = true
		return 0, io.EOF
	case read := <-rw.readch:
		n := copy(b, read.b)
		if n == len(read.b) {
			if read.err == io.EOF {
				rw.eof = true
			}
			return n, read.err
		}
		rw.left = read.b[n:]
		rw.readerr = read.err
		return n, nil
	}
}

type rwAck struct {
	hdr http.Header
	str *stream
}

// sends a synreply frame
func (rw *responseWriter) sendAck(hdr http.Header) {
	if rw.isClosed() {
		return
	}
	select {
	case rw.ackch <- rwAck{hdr, rw.stream}:
	case _ = <-rw.reset:
		rw.closed = true
	}
}

// closes stream. Sends a data frame with the Fin flag set
func (rw *responseWriter) close() error {
	if rw.isClosed() {
		return nil
	}
	select {
	case rw.finch <- rw.stream:
	case _ = <-rw.reset:
	}
	rw.closed = true
	return nil
}
