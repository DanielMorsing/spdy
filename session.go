// Copyright (c) 2013, Daniel Morsing
// For more information, see the LICENSE file

package spdy

import (
	"bufio"
	"github.com/DanielMorsing/spdy/framing"
	"io"
	"log"
	"net"
	"time"
)

const noGoAway framing.GoAwayStatus = 0xff

// session represents a spdy session
type session struct {
	// a map of active streams. If a stream has been closed on both ends
	// it will be removed from this map.
	streams map[framing.StreamId]*stream

	// the maximum number of concurrent streams
	maxStreams uint32
	framer     *framing.Framer
	conn       net.Conn
	// buffered input output, since the Framer does small writes.
	br *bufio.Reader

	framech chan framing.Frame

	// channel to bail out when closed
	closech chan struct{}

	finch chan *stream

	// channel to signal to the serving goroutine to close
	closemsgch chan framing.GoAwayStatus

	// the server that initiated this session
	server *Server

	// the initial window size that streams should be created with
	initialSendWindow uint32
	receiveWindow     uint32

	// last received streamid from client
	lastClientStream framing.StreamId

	// the infamous outframer
	of *outFramer
}

// newSession initiates a SPDY session
func (ps *Server) newSession(c net.Conn) (*session, error) {
	s := &session{
		maxStreams:        100,
		closech:           make(chan struct{}),
		conn:              c,
		server:            ps,
		initialSendWindow: 64 << 10, // 64 kb
		receiveWindow:     64 << 10,
		streams:           make(map[framing.StreamId]*stream),
		framech:           make(chan framing.Frame),
		closemsgch:        make(chan framing.GoAwayStatus, 1),
		finch:             make(chan *stream),
	}
	s.br = bufio.NewReader(c)
	bw := bufio.NewWriter(c)
	s.conn = c

	f, err := framing.NewFramer(bw, s.br)
	if err != nil {
		return nil, err
	}
	s.framer = f
	of := &outFramer{
		framer:       f,
		conn:         c,
		bw:           bw,
		writetimeout: ps.WriteTimeout,
		session:      s,
		requestch:    make(chan frameRq),
		releasech:    make(chan struct{}),
	}
	s.of = of
	go of.prioritize()
	return s, nil
}

// closeStream closes a stream and removes it from the map if both ends have been closed.
// if the closing operation was caused by a reset, just remove the stream, regardless of state.
func (s *session) closeStream(str *stream) {
	close(str.reset)
	delete(s.streams, str.id)
}

func (s *session) close(status framing.GoAwayStatus) {
	select {
	case s.closemsgch <- status:
	default:
	}
}

func (s *session) doclose(status framing.GoAwayStatus) {
	for _, str := range s.streams {
		close(str.reset)
	}

	if status != noGoAway {
		goAway := framing.GoAwayFrame{
			LastGoodStreamId: s.lastClientStream,
			Status:           status,
		}
		// we're going to die after this send.
		// no reason to check the error
		// also we can't be blocked on this, so give timeout.
		// using the general purpose method on the outframer adjusts the
		// timeout, so go around it in this case.
		s.of.get(nil)
		s.conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		s.of.framer.WriteFrame(&goAway)
		s.of.bw.Flush()
	}
	close(s.closech)
	s.conn.Close()
}

func (s *session) serve() {
	go s.readFrames()
	for {
		select {
		case f := <-s.framech:
			s.dispatch(f)
		case status := <-s.closemsgch:
			s.doclose(status)
			return
		case str := <-s.finch:
			s.closeStream(str)
		}
	}
}

func (s *session) readFrames() {
	for {
		if d := s.server.ReadTimeout; d != 0 {
			s.conn.SetReadDeadline(time.Now().Add(d))
		}
		frame, err := s.framer.ReadFrame()
		if err != nil {
			status := framing.GoAwayInternalError
			if ne, ok := err.(net.Error); ok {
				if ne.Timeout() {
					status = framing.GoAwayOK
				} else if !ne.Temporary() {
					status = noGoAway
				}
			} else if err == io.EOF {
				status = noGoAway
			}
			s.close(status)
			return
		}
		select {
		case s.framech <- frame:
		case <-s.closech:
			return
		}
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
	case *framing.InDataFrame:
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
			s.initialSendWindow = flag.Value
		}
	}
}

// handleReq initiates a stream
func (s *session) handleReq(syn *framing.SynStreamFrame) {
	if s.lastClientStream == 0 {
		if syn.StreamId != 1 {
			s.close(framing.GoAwayProtocolError)
			return
		}
		s.lastClientStream = 1
	} else if syn.StreamId != s.lastClientStream+2 {
		s.close(framing.GoAwayProtocolError)
		return
	} else {
		s.lastClientStream += 2
	}

	if len(s.streams) == int(s.maxStreams) {
		s.sendRst(syn.StreamId, framing.RefusedStream)
		return
	}

	stream := newStream(s, syn)
	s.streams[stream.id] = stream
	stream.handleReq(s.server.Handler, syn.Headers)
}

func (s *session) handleRst(rst *framing.RstStreamFrame) {
	str, ok := s.streams[rst.StreamId]
	if !ok {
		// stream no longer active.
		return
	}
	s.closeStream(str)
}

func (s *session) handlePing(ping *framing.PingFrame) {
	if ping.Id%2 == 0 {
		// server ping from client, spec says ignore.
		return
	}
	s.of.ping(ping)
}

func (s *session) handleWindowUpdate(upd *framing.WindowUpdateFrame) {
	str, ok := s.streams[upd.StreamId]
	if !ok {
		// received window update for closed stream
		// spec says do nothing.
		return
	}
	str.mu.Lock()
	defer str.mu.Unlock()
	newWindow := str.sendWindow + upd.DeltaWindowSize
	if newWindow&1<<32 != 0 {
		s.sendRst(str.id, framing.FlowControlError)
		return
	}
	oldwind := str.sendWindow
	str.sendWindow = newWindow
	if oldwind == 0 {
		select {
		case str.windowUpdate <- struct{}{}:
		default:
		}
	}
}

func (s *session) sendRst(id framing.StreamId, status framing.RstStreamStatus) {
	f := &framing.RstStreamFrame{
		StreamId: id,
		Status:   status,
	}
	str, ok := s.streams[id]
	if ok {
		s.closeStream(str)
	}
	err := s.of.sendFrame(nil, f)
	if err != nil {
		log.Println(err)
	}
}

func (s *session) handleData(data *framing.InDataFrame) {
	stream, ok := s.streams[data.StreamId]
	if !ok {
		s.sendRst(data.StreamId, framing.InvalidStream)
		err := data.Close()
		if err != nil {
			s.close(framing.GoAwayInternalError)
		}
		return
	}
	err := s.readData(stream, data)
	if err != nil {
		s.close(framing.GoAwayInternalError)
		return
	}
	err = data.Close()
	if err != nil {
		s.close(framing.GoAwayInternalError)
	}

}

func (s *session) readData(stream *stream, data *framing.InDataFrame) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.receivedFin {
		s.sendRst(stream.id, framing.StreamAlreadyClosed)
		return nil
	}
	if data.Length > stream.receiveWindow {
		s.sendRst(stream.id, framing.FlowControlError)
		return nil
	}
	stream.buf.Grow(int(stream.receiveWindow))
	stream.receiveWindow -= data.Length
	_, err := stream.buf.ReadFrom(data)
	if err != nil {
		return err
	}
	stream.receivedFin = data.Flags&framing.DataFlagFin != 0
	select {
	case stream.recvData <- struct{}{}:
	default:
	}
	return nil
}
