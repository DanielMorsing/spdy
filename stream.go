// Copyright (c) 2013, Daniel Morsing
// For more information, see the LICENSE file

package spdy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"github.com/DanielMorsing/spdy/framing"
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
	id framing.StreamId

	// window size
	sendWindow    uint32
	receiveWindow uint32
	windowOffset  uint32
	priority      uint8
	reset         chan struct{}
	recvData      chan struct{}
	windowUpdate  chan struct{}
	session       *session
	receivedFin   bool
	closed        bool
	buf           bytes.Buffer
	
	// channel to communicate with outframer.
	ofchan chan struct{}

	// mutex to protect the window variables
	mu sync.Mutex
}

func newStream(sess *session, syn *framing.SynStreamFrame) *stream {
	s := &stream{
		id:            syn.StreamId,
		priority:      syn.Priority,
		sendWindow:    sess.initialSendWindow,
		receiveWindow: sess.receiveWindow,
		reset:         make(chan struct{}),
		session:       sess,
		windowUpdate:  make(chan struct{}, 1),
		ofchan: make(chan struct{}, 1),
	}
	if syn.CFHeader.Flags&framing.ControlFlagFin != 0 {
		s.receivedFin = true
	} else {
		s.recvData = make(chan struct{}, 1)
	}
	return s

}

func (str *stream) isClosed() bool {
	if str.closed {
		return true
	}
	select {
	case <-str.reset:
		str.closed = true
		return true
	default:
		return false
	}
}

func (str *stream) handleReq(hnd http.Handler, hdr http.Header) {
	rw := responseWriter{
		stream: str,
		header: make(http.Header),
	}

	req, err := mkrequest(hdr)
	if err != nil {
		// spdy is weird in the case where a stream req doesn't have the right headers.
		// you need to send a 400 reply, rather than a stream reset.
		go func() {
			defer rw.close()
			rw.WriteHeader(http.StatusBadRequest)

		}()
		return
	}
	rw.bufw = bufio.NewWriter(str)
	if !str.receivedFin {
		req.Body = ioutil.NopCloser(str)
	}
	go func() {
		defer rw.close()
		hnd.ServeHTTP(&rw, req)
	}()
}

var requiredHeaders = [...]string{
	":method",
	":path",
	":version",
	":scheme",
	":host",
}

func mkrequest(hdr http.Header) (*http.Request, error) {

	for _, r := range requiredHeaders {
		if _, ok := hdr[r]; !ok {
			return nil, errors.New("invalid headers")
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
	return req, nil
}

func (str *stream) Write(b []byte) (int, error) {
	n := 0
	var err error
	for {
		if len(b) == 0 {
			break
		}

		tb, blocked := str.truncateBuffer(b)
		if blocked {
			err := str.waitWindow()
			if err != nil {
				return n, err
			}
			continue
		}

		err := str.session.of.write(str, tb)
		if err != nil {
			return n, err
		}
		n += len(tb)
		b = b[len(tb):]

	}
	return n, err
}

func (str *stream) waitWindow() error {
	select {
	case <-str.windowUpdate:
		return nil
	case <-str.reset:
		return io.ErrClosedPipe
	}
}

func (str *stream) Read(b []byte) (int, error) {
	for {
		n, gotData, err := str.tryRead(b)
		if gotData {
			return n, err
		}
		select {
		case <-str.recvData:
		case <-str.reset:
			return 0, io.ErrClosedPipe
		}
	}
}

func (str *stream) tryRead(b []byte) (int, bool, error) {
	str.mu.Lock()
	defer str.mu.Unlock()
	n, err := str.buf.Read(b)
	if str.receivedFin {
		// have everything buffered, no need to update window size
		return n, true, err
	}
	str.windowOffset += uint32(n)
	if err != io.EOF {
		if str.receiveWindow < 4096 {
			str.receiveWindow += str.windowOffset
			str.session.of.sendWindowUpdate(str, str.windowOffset)
			str.windowOffset = 0
		}
		return n, true, err
	}
	return 0, false, nil
}

// this method truncates the buffer to a more reasonable size for a spdy frame.
// It also reports whether we're blocked on the sending window.
func (str *stream) truncateBuffer(b []byte) ([]byte, bool) {
	str.mu.Lock()
	defer str.mu.Unlock()

	if str.sendWindow == 0 {
		return b, true
	}

	// limit frame size so that multiple streams are more efficiently multiplexed
	// on to the connection.
	if len(b) > 2000 {
		b = b[:2000]
	}
	if len(b) > int(str.sendWindow) {
		b = b[:str.sendWindow]
	}
	str.sendWindow -= uint32(len(b))
	return b, false
}

// sends a synreply frame
func (str *stream) sendAck(hdr http.Header) {
	if str.isClosed() {
		return
	}
	str.session.of.reply(str, hdr)
}

func (str *stream) sendFin() {
	if str.isClosed() {
		return
	}
	str.session.of.sendFin(str)
	str.closed = true
	select {
	case str.session.finch <- str:
	case <-str.reset:
	}
}

// responseWriter is the type passed to the request handler.
// it is only touched by the handling goroutine.
type responseWriter struct {
	stream        *stream
	headerWritten bool
	header        http.Header

	bufw *bufio.Writer
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.headerWritten {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.bufw.Write(b)
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
	rw.stream.sendAck(rw.header)
	rw.headerWritten = true
}

func (rw *responseWriter) close() {
	if !rw.headerWritten {
		rw.WriteHeader(http.StatusOK)
	}
	rw.bufw.Flush()
	rw.stream.sendFin()
}
