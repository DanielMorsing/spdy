// Copyright (c) 2013, Daniel Morsing
// For more information, see the LICENSE file

package spdy

import (
	"bufio"
	"container/heap"
	"github.com/DanielMorsing/spdy/framing"
	"io"
	"log"
	"net"
	"net/http"
	"time"
)

// Beware: the outframer.
// This is probably the scariest part of this entire package.
// Big picture, it's a mutex that prioritizes based on stream priority, for use with the outgoing part of the framer.
// Because so many things can happen while stream is blocked, this code has to respond to all of them.
//
// Consider yourself warned.
type outFramer struct {
	framer  *framing.Framer
	session *session
	bw      *bufio.Writer
	conn    net.Conn

	writetimeout time.Duration

	requestch chan frameRq
	releasech chan struct{}

	// memory caches for commonly used frames
	data   framing.DataFrame
	syn    framing.SynReplyFrame
	window framing.WindowUpdateFrame
}

// writeFrame writes a frame to the connection and flushes the outgoing buffered IO
func (of *outFramer) writeFrame(f framing.Frame) error {
	if d := of.writetimeout; d != 0 {
		of.conn.SetWriteDeadline(time.Now().Add(d))
	}
	err := of.framer.WriteFrame(f)
	if err != nil {
		return err
	}
	return of.bw.Flush()
}

// writeError handles any error that happens when writing to the connection
// it returns whether the error was terminal.
func (of *outFramer) writeError(err error) bool {
	switch e := err.(type) {
	case net.Error:
		// network error when writing frame. Just close without goaway frame.
		of.session.close(noGoAway)
	case *framing.Error:
		// framing will call you out on a couple of errors
		log.Println(e)
		of.session.close(framing.GoAwayInternalError)
	}
	return true
}

// Plumbing for prioritizing requesters that need the outframer
type frameRq struct {
	str    *stream
	ofchan chan *outFramer
}

type prioQueue []frameRq

func (ph prioQueue) Len() int            { return len(ph) }
func (ph prioQueue) Swap(i, j int)       { ph[i], ph[j] = ph[j], ph[i] }
func (ph *prioQueue) Push(i interface{}) { *ph = append(*ph, i.(frameRq)) }
func (ph *prioQueue) Pop() interface{} {
	i := (*ph)[len(*ph)-1]
	*ph = (*ph)[:len(*ph)-1]
	return i
}
func (ph prioQueue) Less(i, j int) bool {
	pi := &ph[i]
	pj := &ph[j]
	return frameLess(pi, pj)
}

func (ph *prioQueue) popRq() frameRq {
	if ph.Len() == 0 {
		return frameRq{}
	}
	return heap.Pop(ph).(frameRq)
}

func frameLess(a, b *frameRq) bool {
	if a.str == nil {
		return b.str != nil
	} else if b.str == nil {
		return false
	}
	return a.str.priority < b.str.priority
}


// prioritize does the heavy lifting in the prioritization algorithm.
func (of *outFramer) prioritize() {
	var prio prioQueue
	var nextrq frameRq
	var outchan chan *outFramer
	var reset chan struct{}
	var taken bool

	for {
		if !taken {
			outchan = nextrq.ofchan
			// if we're dealing with a stream,
			// listen for resets.
			if nextrq.str != nil {
				reset = nextrq.str.reset
			} else {
				reset = nil
			}
		} else {
			// framer is taken, don't try to fufill pending requests
			outchan = nil
			reset = nil
		}

		select {

		case outchan <- of:
			nextrq = prio.popRq()
			taken = true
		case <-reset:
			nextrq = prio.popRq()

		case rq := <-of.requestch:
			if nextrq.ofchan == nil {
				// no one next in line for the framer
				nextrq = rq
				continue
			}
			// if the incoming request is prioritized over the one pending,
			// switch the requests and put the old req onto the heap
			if frameLess(&rq, &nextrq) {
				nextrq, rq = rq, nextrq
			}
			heap.Push(&prio, rq)
		case <-of.releasech:
			taken = false
		case <-of.session.closech:
			return
		}
	}
}

// Get the outFramer. If str is nil, it will be prioritized over all other requests.
// nil is normally used by the session goroutine.
func (of *outFramer) get(str *stream) error {
	var reset chan struct{}
	ch := make(chan *outFramer, 1)
	rq := frameRq{str, ch}
	if str != nil {
		reset = str.reset
	}

	select {
	case of.requestch <- rq:
	case <-reset:
		return io.ErrClosedPipe
	case <-of.session.closech:
		return io.ErrClosedPipe
	}
	select {
	case <-ch:
	case <-reset:
		return io.ErrClosedPipe
	case <-of.session.closech:
		return io.ErrClosedPipe
	}
	return nil
}

// release the outframer
func (of *outFramer) release() {
	select {
	case of.releasech <- struct{}{}:
	case <-of.session.closech:
	}
}

// The following functions are the main interface to the outframer.
// Using them is recommended since they reuse buffers internal to the outFramer
// and ensures that other requesters are allowed access to the framer.

// Write a data frame to the framer
func (of *outFramer) write(str *stream, b []byte) error {
	err := of.get(str)
	if err != nil {
		return err
	}
	defer of.release()

	of.data.StreamId = str.id
	of.data.Data = b
	of.data.Flags = 0
	err = of.writeFrame(&of.data)
	if err != nil {
		of.writeError(err)
	}
	return err
}

// Write a ping frame to the framer.
func (of *outFramer) ping(ping *framing.PingFrame) {
	go func() {
		err := of.get(nil)
		if err != nil {
			return
		}
		defer of.release()
		err = of.writeFrame(ping)
		if err != nil {
			of.writeError(err)
		}
	}()
}

// Write a Synstream frame to the framer
func (of *outFramer) reply(str *stream, hdr http.Header) error {

	err := of.get(str)
	if err != nil {
		return err
	}
	defer of.release()

	of.syn.StreamId = str.id
	of.syn.CFHeader.Flags = 0
	of.syn.Headers = hdr

	err = of.writeFrame(&of.syn)
	if err != nil {
		of.writeError(err)
	}
	return err
}

// Write an empty data frame with the fin flag set
func (of *outFramer) sendFin(str *stream) error {
	err := of.get(str)
	if err != nil {
		return err
	}
	defer of.release()

	of.data.Flags = framing.DataFlagFin
	of.data.Data = nil
	of.data.StreamId = str.id

	err = of.writeFrame(&of.data)
	if err != nil {
		of.writeError(err)
	}
	return err
}

// Write a window update frame
func (of *outFramer) sendWindowUpdate(str *stream, delta uint32) error {
	err := of.get(str)
	if err != nil {
		return err
	}
	defer of.release()

	of.window.DeltaWindowSize = delta
	of.window.StreamId = str.id

	err = of.writeFrame(&of.window)
	if err != nil {
		of.writeError(err)
	}
	return err
}

// send an arbitrary frame to the wire
func (of *outFramer) sendFrame(str *stream, f framing.Frame) error {
	err := of.get(str)
	if err != nil {
		return err
	}
	defer of.release()

	err = of.writeFrame(f)
	if err != nil {
		of.writeError(err)
	}
	return err
}
