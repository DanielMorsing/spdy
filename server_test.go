// Copyright (c) 2013, Daniel Morsing
// For more information, see the LICENSE file

// high level tests.

package spdy_test

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"github.com/DanielMorsing/spdy"
	"github.com/DanielMorsing/spdy/framing"
	"net"
	"net/http"
	"testing"
	"time"
)

func init() {
	var response = `
	<html>
	<head></head>
	<body>
	<form action="hej" method="post">
	First name: <input type="text" name="firstname"><br>
	Last name: <input type="text" name="lastname">

	<input type="submit" value="Submit"/>
	</form>
	</body>
	</html>
	`
	ds := http.DefaultServeMux
	ds.HandleFunc("/", func(rw http.ResponseWriter, rq *http.Request) {
		rw.WriteHeader(200)
		<-corkch
		rw.Write([]byte(response))
	})
	ds.HandleFunc("/hej", func(rw http.ResponseWriter, rq *http.Request) {
		rq.ParseForm()
		rw.WriteHeader(200)
		fmt.Fprintf(rw, "your name is %s %s\n", rq.Form["firstname"][0], rq.Form["lastname"][0])
	})
	srv := &spdy.Server{
		Addr:    "localhost:4444",
		Handler: ds,
	}
	go srv.ListenAndServe()
	// a complete hack, but it's needed so that the connection isn't refused.
	time.Sleep(200 * time.Millisecond)
}

var corkch = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

func cork() {
	corkch = make(chan struct{})
}

func uncork() {
	corkch <- struct{}{}
	close(corkch)
}

var clientConfig = tls.Config{
	InsecureSkipVerify: true,
	NextProtos:         []string{"spdy/3"},
}

func newClientSession() (*framing.Framer, net.Conn) {
	n, err := net.Dial("tcp", "localhost:4444")
	if err != nil {
		panic(err)
	}
	framer, err := framing.NewFramer(n, n)
	if err != nil {
		panic(err)
	}
	return framer, n
}

func goldenHeader() http.Header {
	m := make(http.Header)
	m[":host"] = []string{"localhost:4444"}
	m[":path"] = []string{"/"}
	m[":scheme"] = []string{"https"}
	m[":version"] = []string{"HTTP/1.1"}
	m["User-Agent"] = []string{"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/28.0.1500.95 Safari/537.36"}
	m["Accept"] = []string{"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"}
	m[":method"] = []string{"GET"}
	m["Accept-Encoding"] = []string{"gzip,deflate,sdch"}
	m["Accept-Language"] = []string{"en-US,en;q=0.8,da;q=0.6"}
	return m
}

func makeRequest() *framing.SynStreamFrame {
	return &framing.SynStreamFrame{
		framing.ControlFrameHeader{
			Flags: framing.ControlFlagFin,
		},
		1,
		0,
		0,
		0,
		goldenHeader(),
	}
}

func goldenSettings() *framing.SettingsFrame {
	return &framing.SettingsFrame{
		FlagIdValues: []framing.SettingsFlagIdValue{
			framing.SettingsFlagIdValue{0, 4, 1000},
			framing.SettingsFlagIdValue{0, 7, 10485760},
		},
	}
}

func frameRead(t *testing.T, f *framing.Framer) framing.Frame {
	frame, err := f.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func frameWrite(t *testing.T, f *framing.Framer, frame framing.Frame) {
	err := f.WriteFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
}

func TestPing(t *testing.T) {
	f, n := newClientSession()
	defer n.Close()

	p := framing.PingFrame{Id: 1}
	frameWrite(t, f, &p)

	retf := frameRead(t, f)
	if retp, ok := retf.(*framing.PingFrame); ok {
		if retp.Id != 1 {
			t.Error("ping id returned is not the one sent")
		}
	} else {
		t.Error("Received non ping frame")
	}
}

func TestSettings(t *testing.T) {
	f, n := newClientSession()
	defer n.Close()

	set := goldenSettings()
	frameWrite(t, f, set)
}

func TestBasicRequest(t *testing.T) {
	f, n := newClientSession()
	defer n.Close()

	set := goldenSettings()
	frameWrite(t, f, set)
	syn := makeRequest()
	frameWrite(t, f, syn)

	r := frameRead(t, f)
	rp, ok := r.(*framing.SynReplyFrame)
	if !ok {
		t.Fatal("did not get reply frame")
	}
	if rp.StreamId != 1 {
		t.Fatal("Wrong StreamId")
	}
	for {
		frame := frameRead(t, f)
		d, ok := frame.(*framing.DataFrame)
		if !ok {
			t.Fatal("non data frame received")
		}
		// got fin
		if d.Flags != 0 {
			break
		}
	}
}

func TestReset(t *testing.T) {
	f, n := newClientSession()
	defer n.Close()

	set := goldenSettings()
	frameWrite(t, f, set)
	syn := makeRequest()
	cork()
	frameWrite(t, f, syn)
	r := frameRead(t, f)
	_, ok := r.(*framing.SynReplyFrame)
	if !ok {
		t.Fatal("did not get reply frame")
	}
	rst := framing.RstStreamFrame{
		StreamId: 1,
		Status:   framing.Cancel,
	}
	frameWrite(t, f, &rst)

	// there is a acknowledged race condition here.
	// where the server may send inflight dataframes
	// after a rst has been sent.
	//
	// make sure that reset has been received by sending a ping
	p := framing.PingFrame{Id: 1}
	frameWrite(t, f, &p)
	retf := frameRead(t, f)
	if _, ok := retf.(*framing.PingFrame); !ok {
		t.Error("Received non ping frame")
	}

	var ch = make(chan framing.Frame, 1)
	go func() {
		for {
			f, err := f.ReadFrame()
			if err != nil {
				return
			}
			ch <- f
		}
	}()
	uncork()

	timech := time.After(500 * time.Millisecond)
	select {
	case <-ch:
		t.Fatal("got frame after reset")
	case <-timech:
		return
	}
}

func TestWindowLimit(t *testing.T) {
	f, n := newClientSession()
	defer n.Close()

	set := goldenSettings()
	for i := range set.FlagIdValues {
		if set.FlagIdValues[i].Id == framing.SettingsInitialWindowSize {
			set.FlagIdValues[i].Value = 1
		}
	}
	frameWrite(t, f, set)

	syn := makeRequest()
	frameWrite(t, f, syn)

	frame := frameRead(t, f)
	_, ok := frame.(*framing.SynReplyFrame)
	if !ok {
		t.Fatal("did not get reply frame")
	}

	frame = frameRead(t, f)
	d, ok := frame.(*framing.DataFrame)
	if !ok {
		t.Fatal("did not get data frame")
	}
	if len(d.Data) != 1 {
		t.Fatal("Data frame larger than window")
	}
	var ch = make(chan framing.Frame, 1)
	go func() {
		f := frameRead(t, f)
		ch <- f
	}()
	select {
	case <-ch:
		t.Error("got data even though it's supposed to be blocked on window")
	case <-time.After(200 * time.Millisecond):
		wnd := framing.WindowUpdateFrame{
			StreamId:        1,
			DeltaWindowSize: 1 << 20,
		}
		frameWrite(t, f, &wnd)
	}
	select {
	case <-ch:
	case <-time.After(500 * time.Millisecond):
		t.Error("didn't get data after window unblocked")
	}
}

func TestResetWhileWindowblocked(t *testing.T) {
	f, n := newClientSession()
	defer n.Close()

	set := goldenSettings()
	for i := range set.FlagIdValues {
		if set.FlagIdValues[i].Id == framing.SettingsInitialWindowSize {
			set.FlagIdValues[i].Value = 1
		}
	}
	frameWrite(t, f, set)

	syn := makeRequest()
	frameWrite(t, f, syn)

	frame := frameRead(t, f)
	_, ok := frame.(*framing.SynReplyFrame)
	if !ok {
		t.Fatal("did not get reply frame")
	}
	frame = frameRead(t, f)
	d, ok := frame.(*framing.DataFrame)
	if !ok {
		t.Fatal("did not get data frame")
	}
	if len(d.Data) != 1 {
		t.Fatal("Data frame larger than window")
	}

	var ch = make(chan framing.Frame, 1)
	go func() {
		f, _ := f.ReadFrame()
		ch <- f
	}()
	select {
	case <-ch:
		t.Error("got data even though it's supposed to be blocked on window")
	case <-time.After(200 * time.Millisecond):
		rst := framing.RstStreamFrame{
			StreamId: 1,
			Status:   framing.Cancel,
		}
		frameWrite(t, f, &rst)
	}
}

func TestPostRequest(t *testing.T) {
	f, n := newClientSession()
	defer n.Close()

	set := goldenSettings()
	frameWrite(t, f, set)
	syn := makeRequest()
	syn.Headers[":path"] = []string{"/hej"}
	syn.Headers[":method"] = []string{"POST"}
	syn.Headers["Content-Type"] = []string{"application/x-www-form-urlencoded"}
	syn.CFHeader.Flags = 0
	frameWrite(t, f, syn)
	d := &framing.DataFrame{}
	d.StreamId = 1
	d.Flags = framing.DataFlagFin
	d.Data = []byte("firstname=daniel&lastname=morsing")
	frameWrite(t, f, d)

	r := frameRead(t, f)
	rp, ok := r.(*framing.SynReplyFrame)
	if !ok {
		t.Fatal("did not get reply frame")
	}
	if rp.StreamId != 1 {
		t.Fatal("Wrong StreamId")
	}

	frame := frameRead(t, f)
	d, ok = frame.(*framing.DataFrame)
	if !ok {
		t.Fatal("non data frame received:", frame)
	}
	exp := []byte("your name is daniel morsing\n")
	if !bytes.Equal(d.Data, exp) {
		t.Errorf("expected %s; got %s", exp, d.Data)
	}
}

func TestInvalidStreamId(t *testing.T) {
	f, n := newClientSession()
	defer n.Close()

	syn := makeRequest()
	syn.StreamId = 2

	frameWrite(t, f, syn)
	goaway := frameRead(t, f)
	g, ok := goaway.(*framing.GoAwayFrame)
	if !ok {
		// didn't get reset
		t.Errorf("did not get goaway frame. got %T instead", g)
		return
	}
	if g.Status != framing.GoAwayProtocolError {
		t.Error("Wrong error code")
	}
	_, err := f.ReadFrame()
	if err == nil {
		t.Error("Session not closed")
	}
}
