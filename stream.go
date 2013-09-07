// Copyright (c) 2013, Daniel Morsing
// For more information, see the LICENSE file

package spdy

import (
	"bufio"
	"bytes"
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
	reset         chan error
	retch         chan int
	session       *session
	receivedFin   bool
	sentFin       bool
	blocked       bool
	closed        bool
	buf           bytes.Buffer
	cond          *sync.Cond

	// mutex to protect the window variable
	mu sync.Mutex
}

func newStream(sess *session, syn *framing.SynStreamFrame) *stream {
	s := &stream{
		id:            syn.StreamId,
		priority:      syn.Priority,
		sendWindow:    sess.initialSendWindow,
		receiveWindow: sess.receiveWindow,
		reset:         make(chan error),
		retch:         make(chan int),
		session:       sess,
	}
	s.cond = sync.NewCond(&s.mu)
	if syn.CFHeader.Flags&framing.ControlFlagFin != 0 {
		s.receivedFin = true
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
	req := mkrequest(hdr)
	rw := responseWriter{
		stream: str,
		header: make(http.Header),
	}
	rw.bufw = bufio.NewWriter(str)
	if !str.receivedFin {
		req.Body = ioutil.NopCloser(str)
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

// types for ferrying data back and forth to the session
type streamWrite struct {
	b   []byte
	str *stream
}

func (str *stream) Write(b []byte) (int, error) {
	n := 0
	var err error
	sw := streamWrite{b, str}
	ch := str.session.streamch
	retch := chan int(nil)
	for {
		if str.isClosed() {
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
			retch = str.retch
			ch = nil
			continue
		case ret := <-retch:
			n += ret
			b = b[ret:]
			ch = str.session.streamch
			retch = nil
			continue
		case err = <-str.reset:
			str.closed = true
			return n, io.ErrClosedPipe
		}
	}
	return n, err
}

type windowupdate struct {
	d  int
	id framing.StreamId
}

func (str *stream) Read(b []byte) (int, error) {
	str.mu.Lock()
	defer str.mu.Unlock()
	for {
		n, err := str.buf.Read(b)
		if str.receivedFin {
			// have everything buffered, no need to update window size
			return n, err
		}
		str.windowOffset += uint32(n)
		if err != io.EOF {
			if str.windowOffset > 4096 {
				select {
				case str.session.windowOut <- windowupdate{int(str.windowOffset), str.id}:
					str.receiveWindow += str.windowOffset
					str.windowOffset = 0
				case <-str.reset:
					return n, io.ErrClosedPipe
				}
			}
			return n, err
		}
		gotNew := make(chan struct{}, 1)
		go func() {
			str.cond.Wait()
			gotNew <- struct{}{}
		}()
		select {
		case <-gotNew:
		case <-str.reset:
			// make sure we don't leave a goroutine around
			str.cond.Signal()
			return 0, io.ErrClosedPipe
		}
	}
}

// builds a dataframe for the byte slice and updates the window.
// if the window is empty, set the blocked field and return 0
// this function is called from the session goroutine.
func (str *stream) buildDataframe(data *framing.DataFrame, b []byte) int {
	data.StreamId = str.id
	data.Flags = 0
	str.mu.Lock()
	defer str.mu.Unlock()
	if str.sendWindow == 0 {
		str.blocked = true
		return 0
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
	data.Data = b
	return len(b)
}

type strAck struct {
	hdr http.Header
	str *stream
}

// sends a synreply frame
func (str *stream) sendAck(hdr http.Header) {
	if str.isClosed() {
		return
	}
	select {
	case str.session.ackch <- strAck{hdr, str}:
	case _ = <-str.reset:
		str.closed = true
	}
}

func (str *stream) sendFin() {
	if str.isClosed() {
		return
	}
	select {
	case str.session.finch <- str:
	case _ = <-str.reset:
	}
	str.closed = true
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

func (rw *responseWriter) close() error {
	if !rw.headerWritten {
		rw.WriteHeader(http.StatusOK)
	}
	rw.bufw.Flush()
	rw.stream.sendFin()
	return nil
}
