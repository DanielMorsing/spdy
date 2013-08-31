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
	"sync"
	"time"
)

// session represents a spdy session
type session struct {
	// a map of active streams. If a stream has been closed on both ends
	// it will be removed from this map.
	streams map[framing.StreamId]*stream

	// Protects the data that both sending and receiving side uses,
	// namely the streams and the close channel
	mu sync.Mutex

	// the maximum number of concurrent streams
	maxStreams uint32
	framer     *framing.Framer
	conn       net.Conn
	// buffered input output, since the Framer does small writes.
	br *bufio.Reader
	bw *bufio.Writer

	// channels to communicate with streams
	streamch chan streamWrite
	ackch    chan rwAck
	finch    chan *stream

	// channels to communicate between the 2 goroutines
	// that make up the server.
	pingch    chan *framing.PingFrame
	sendReset chan *framing.RstStreamFrame
	windowOut chan windowupdate

	// channel to bail out of the main event loop when closed
	closech chan struct{}

	// the server that initiated this session
	server *Server

	// the initial window size that streams should be created with
	initialWindow uint32

	// last received streamid from client
	lastClientStream framing.StreamId

	// memory caches for commonly used frames
	data   framing.DataFrame
	reply  framing.SynReplyFrame
	window framing.WindowUpdateFrame
}

// newSession initiates a SPDY session
func (ps *Server) newSession(c net.Conn) (*session, error) {
	s := &session{
		maxStreams:    100,
		closech:       make(chan struct{}),
		streamch:      make(chan streamWrite),
		ackch:         make(chan rwAck),
		finch:         make(chan *stream),
		pingch:        make(chan *framing.PingFrame, 1),
		sendReset:     make(chan *framing.RstStreamFrame, 1),
		windowOut:     make(chan windowupdate, 10),
		conn:          c,
		server:        ps,
		initialWindow: 64 << 10, // 64 kb
		streams:       make(map[framing.StreamId]*stream),
	}
	s.br = bufio.NewReader(c)
	s.bw = bufio.NewWriter(c)
	s.conn = c

	f, err := framing.NewFramer(s.bw, s.br)
	if err != nil {
		return nil, err
	}
	s.framer = f
	return s, nil
}

func (s *session) getStream(id framing.StreamId) (*stream, bool) {
	s.mu.Lock()
	str, ok := s.streams[id]
	s.mu.Unlock()
	return str, ok
}

func (s *session) putStream(id framing.StreamId, str *stream) {
	s.mu.Lock()
	s.streams[id] = str
	s.mu.Unlock()
}

func (s *session) numStreams() int {
	s.mu.Lock()
	l := len(s.streams)
	s.mu.Unlock()
	return l
}

func (s *session) closeStream(str *stream, isReset bool) {
	s.mu.Lock()
	str.mu.Lock()

	receivedFin := str.receivedFin
	str.sentFin = !isReset

	select {
	case <-str.reset:
	default:
		close(str.reset)
	}

	str.mu.Unlock()

	if receivedFin || isReset {
		delete(s.streams, str.id)
	}
	s.mu.Unlock()
}

// close shuts down the session and closes all the streams.
func (s *session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closech:
		return
	default:
		close(s.closech)
	}
	for _, str := range s.streams {
		close(str.reset)
	}

	s.bw.Flush()
	s.conn.Close()
}

// sessionError causes a session error. It also closes the session.
func (s *session) sessionError(status framing.GoAwayStatus) {
	goAway := framing.GoAwayFrame{
		LastGoodStreamId: s.lastClientStream,
		Status:           status,
	}
	// we're going to die after this send.
	// no reason to check the error
	s.writeFrame(&goAway)
	s.close()
}

// writeFrame writes a frame to the connection and flushes the outgoing buffered IO
func (s *session) writeFrame(f framing.Frame) error {
	if d := s.server.WriteTimeout; d != 0 {
		s.conn.SetWriteDeadline(time.Now().Add(d))
	}
	err := s.framer.WriteFrame(f)
	if err != nil {
		return err
	}
	return s.bw.Flush()
}

// serve handles events coming from the various streams.
func (s *session) serve() {
	go s.readFrames()
	prio := s.prioritize()
	for {
		select {
		case sd := <-prio:
			n := sd.str.buildDataframe(&s.data, sd.b)
			if n == 0 {
				// window is empty. Wait for update.
				continue
			}
			err := s.writeFrame(&s.data)
			if err != nil {
				s.close()
				return
			}
			select {
			case sd.str.retch <- n:
			case <-sd.str.reset:
			}
		case syn := <-s.ackch:
			makeSynReply(&s.reply, syn.hdr, syn.str)
			err := s.writeFrame(&s.reply)
			if err != nil {
				s.close()
				return
			}
		case fin := <-s.finch:
			makeFin(&s.data, fin.id)
			err := s.writeFrame(&s.data)
			if err != nil {
				s.close()
				return
			}
			s.closeStream(fin, false)
		case f := <-s.pingch:
			err := s.writeFrame(f)
			if err != nil {
				s.close()
				return
			}
		case f := <-s.sendReset:
			err := s.writeFrame(f)
			if err != nil {
				s.close()
				return
			}
		case wo := <-s.windowOut:
			s.window.StreamId = wo.id
			s.window.DeltaWindowSize = uint32(wo.d)
			err := s.writeFrame(&s.window)
			if err != nil {
				s.close()
				return
			}
		case <-s.closech:
			return
		}
	}
}

type prioQueue []streamWrite

func (ph prioQueue) Len() int            { return len(ph) }
func (ph prioQueue) Swap(i, j int)       { ph[i], ph[j] = ph[j], ph[i] }
func (ph *prioQueue) Push(i interface{}) { *ph = append(*ph, i.(streamWrite)) }
func (ph *prioQueue) Pop() interface{} {
	i := (*ph)[len(*ph)-1]
	*ph = (*ph)[:len(*ph)-1]
	return i
}
func (ph prioQueue) Less(i, j int) bool {
	return ph[i].str.priority < ph[j].str.priority
}

// naive prioritization. Since there's no buffering, it schedules only on streams with have pending writes.
// which means that if a prio 6 stream is currently being written and a prio 2 frame comes along,
// the prio 2 frame will be scheduled next, rather than the next frame of the prio 6 stream.
// You could imagine a better solution with some buffering, but it is expensive, and prioritization is best effort
// according to the spec. Doing a better job simply not worth it.

func (s *session) prioritize() chan streamWrite {
	outch := make(chan streamWrite)

	go func() {
		var prio prioQueue
		var rwout streamWrite
		out := outch

		select {
		case rwout = <-s.streamch:
		case <-s.closech:
			return
		}
		for {
			select {
			case out <- rwout:
				if len(prio) == 0 {
					// just sent the last value in the heap.
					// wait until we have a new one
					out = nil
				} else {
					rwout = heap.Pop(&prio).(streamWrite)
				}
			case rw := <-s.streamch:
				if out == nil {
					out = outch
					rwout = rw
					continue
				}
				if rw.str.priority < rwout.str.priority {
					rwout, rw = rw, rwout
				}
				heap.Push(&prio, rw)
			case <-s.closech:
				return
			}
		}
	}()

	return outch
}

// makeSynReply builds a SynReply with a given header and streamid
func makeSynReply(syn *framing.SynReplyFrame, hdr http.Header, str *stream) {
	syn.StreamId = str.id
	syn.CFHeader.Flags = 0
	syn.Headers = hdr
}

// makefin builds a finalizing dataframe with the given StreamId
func makeFin(d *framing.DataFrame, id framing.StreamId) {
	d.Flags = framing.DataFlagFin
	d.StreamId = id
	d.Data = nil
}

// readFrames read frames from the connection and handles
// frames received from the client.
func (s *session) readFrames() {
	for {
		if d := s.server.ReadTimeout; d != 0 {
			s.conn.SetReadDeadline(time.Now().Add(d))
		}
		frame, err := s.framer.ReadFrame()
		if err != nil {
			s.close()
			return
		}
		s.dispatch(frame)
	}
}

// dispatch figures out how to handle frames coming in from the client
func (s *session) dispatch(f framing.Frame) {
	switch fr := f.(type) {
	case *framing.SettingsFrame:
		s.handleSettings(fr)
	case *framing.SynStreamFrame:
		s.handleReq(fr)
	case *framing.RstStreamFrame:
		s.handleRst(fr)
	case *framing.PingFrame:
		s.handlePing(fr)
	case *framing.WindowUpdateFrame:
		s.handleWindowUpdate(fr)
	case *framing.DataFrame:
		s.handleData(fr)
	default:
		log.Println("unhandled frame", fr)
	}
}

// handleSettings adjusts the internal session parameters to what the client has told us
func (s *session) handleSettings(settings *framing.SettingsFrame) {
	for _, flag := range settings.FlagIdValues {
		switch flag.Id {
		case framing.SettingsUploadBandwidth,
			framing.SettingsDownloadBandwidth,
			framing.SettingsRoundTripTime,
			framing.SettingsCurrentCwnd,
			framing.SettingsDownloadRetransRate,
			framing.SettingsClientCretificateVectorSize:
			log.Printf("unsupported setting: %d", flag.Id)
		case framing.SettingsMaxConcurrentStreams:
			s.maxStreams = flag.Value
		case framing.SettingsInitialWindowSize:
			s.initialWindow = flag.Value
		}
	}
}

// handleReq initiates a stream
func (s *session) handleReq(syn *framing.SynStreamFrame) {
	if s.lastClientStream == 0 {
		if syn.StreamId != 1 {
			s.sessionError(framing.GoAwayProtocolError)
			return
		}
		s.lastClientStream = 1
	} else if syn.StreamId != s.lastClientStream+2 {
		s.sessionError(framing.GoAwayProtocolError)
		return
	} else {
		s.lastClientStream += 2
	}

	if s.numStreams() == int(s.maxStreams) {
		s.sendRst(syn.StreamId, framing.RefusedStream)
		return
	}

	stream := newStream(s, syn)
	s.putStream(stream.id, stream)
	stream.handleReq(s.server.Handler, syn.Headers)
}

func (s *session) handleRst(rst *framing.RstStreamFrame) {
	str, ok := s.getStream(rst.StreamId)
	if !ok {
		// stream no longer active.
		return
	}
	s.closeStream(str, true)
}

func (s *session) handlePing(ping *framing.PingFrame) {
	select {
	case s.pingch <- ping:
	case <-s.closech:
	}
}

func (s *session) handleWindowUpdate(upd *framing.WindowUpdateFrame) {
	str, ok := s.getStream(upd.StreamId)
	if !ok {
		// received window update for closed stream
		// spec says do nothing.
		return
	}
	str.mu.Lock()
	defer str.mu.Unlock()
	newWindow := str.window + upd.DeltaWindowSize
	if newWindow&1<<32 != 0 {
		s.sendRst(str.id, framing.FlowControlError)
		return
	}
	str.window = newWindow
	if str.blocked {
		// the stream can only be blocked when it has sent
		// a write request and is ready to receive an error.
		// Framer is being written to by another goroutine,
		// so tell the responseWriter to restart the write.
		str.blocked = false
		select {
		case str.retch <- 0:
		case <-str.reset:
		}
	}

}

func (s *session) sendRst(id framing.StreamId, status framing.RstStreamStatus) {
	f := &framing.RstStreamFrame{
		StreamId: id,
		Status:   status,
	}
	str, ok := s.getStream(id)
	if ok {
		s.closeStream(str, true)
	}
	select {
	case s.sendReset <- f:
	case <-s.closech:
	}
}

type windowupdate struct {
	d  int
	id framing.StreamId
}

func (s *session) handleData(data *framing.DataFrame) {
	stream, ok := s.getStream(data.StreamId)
	if !ok {
		s.sendRst(data.StreamId, framing.InvalidStream)
		return
	}
	// naive window logic. always try to keep the window at the default size.
	select {
	case s.windowOut <- windowupdate{len(data.Data), data.StreamId}:
	case <-stream.reset:
		return
	}
	stream.mu.Lock()
	if stream.receivedFin {
		stream.mu.Unlock()
		s.sendRst(stream.id, framing.StreamAlreadyClosed)
		return
	}

	var err error
	if data.Flags&framing.DataFlagFin != 0 {
		err = io.EOF
		stream.receivedFin = true
	}
	stream.mu.Unlock()
	select {
	case stream.readch <- rwRead{data.Data, err}:
	case <-stream.reset:
		stream.mu.Lock()
		sentFin := stream.sentFin
		stream.mu.Unlock()
		if sentFin {
			// we're receiving data although we've sent a fin.
			// In http, we'd read the entire thing, so that we can reuse the connection,
			// but in the world of framing.we can tell the client to stop sending
			s.sendRst(stream.id, framing.Cancel)
		}
	}
}
