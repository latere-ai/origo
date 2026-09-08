// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package slowproxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// echo is a target that echoes every byte, with the accepted
// connections counted.
func echo(t *testing.T) (net.Listener, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted := new(atomic.Int32)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	return ln, accepted
}

// start serves the proxy on a loopback listener and its control
// endpoint on an httptest server.
func start(t *testing.T, p *Proxy) (data string, control *httptest.Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = p.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); ln.Close(); p.Close() })
	control = httptest.NewServer(p.Handler())
	t.Cleanup(control.Close)
	return ln.Addr().String(), control
}

// roundTrip writes a line through the proxy and reads it back, timing
// the wait for the first byte.
func roundTrip(t *testing.T, addr string) time.Duration {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	started := time.Now()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo through the proxy: %q %v", buf, err)
	}
	return time.Since(started)
}

func TestProxyHoldsTheFirstBytesForTheDelay(t *testing.T) {
	target, _ := echo(t)
	p := New(target.Addr().String())
	data, control := start(t, p)
	if p.Target() != target.Addr().String() || p.Delay() != 0 {
		t.Fatalf("new proxy: %s %s", p.Target(), p.Delay())
	}
	// No delay: at once.
	if took := roundTrip(t, data); took > 500*time.Millisecond {
		t.Fatalf("no delay took %s", took)
	}
	// The control endpoint reports the state and sets the delay.
	resp, err := http.Get(control.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	var st state
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.Target != p.Target() || st.Delay != "0s" {
		t.Fatalf("state %+v", st)
	}
	if err := Set(context.Background(), control.URL, 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if p.Delay() != 300*time.Millisecond {
		t.Fatalf("delay %s", p.Delay())
	}
	if took := roundTrip(t, data); took < 300*time.Millisecond {
		t.Fatalf("delayed connection answered after %s", took)
	}
	// Clearing it restores the pass-through.
	req, _ := http.NewRequest(http.MethodDelete, control.URL+"/delay", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || p.Delay() != 0 {
		t.Fatalf("clear: %d %s", resp.StatusCode, p.Delay())
	}
	if took := roundTrip(t, data); took > 500*time.Millisecond {
		t.Fatalf("after the clear took %s", took)
	}
}

func TestSettingTheDelayClosesPooledConnections(t *testing.T) {
	target, accepted := echo(t)
	p := New(target.Addr().String())
	data, control := start(t, p)
	conn, err := net.Dial("tcp", data)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if err := Set(context.Background(), control.URL, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// The open connection is closed: the next request opens a new one
	// and meets the delay.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("the pooled connection survived the delay change")
	}
	if took := roundTrip(t, data); took < 200*time.Millisecond || accepted.Load() != 2 {
		t.Fatalf("new connection after %s, %d accepted", took, accepted.Load())
	}
}

func TestControlRefusesABadDelayAndSetReportsIt(t *testing.T) {
	p := New("127.0.0.1:1")
	_, control := start(t, p)
	for _, body := range []string{`{"delay":"soon"}`, `{"delay":"-1s"}`, `not json`} {
		req, _ := http.NewRequest(http.MethodPut, control.URL+"/delay", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("%s: %d", body, resp.StatusCode)
		}
	}
	if p.Delay() != 0 {
		t.Fatalf("a refused body changed the delay to %s", p.Delay())
	}
	if err := Set(context.Background(), "http://127.0.0.1:1", time.Second); err == nil {
		t.Fatal("Set against nothing succeeded")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", 500) }))
	defer bad.Close()
	if err := Set(context.Background(), bad.URL, time.Second); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("Set against a failing endpoint: %v", err)
	}
	if err := Set(context.Background(), "::bad", time.Second); err == nil {
		t.Fatal("Set with a bad URL succeeded")
	}
}

func TestProxyEndsWithAnUnreachableTargetAndOnClose(t *testing.T) {
	p := New("127.0.0.1:1")
	data, _ := start(t, p)
	conn, err := net.Dial("tcp", data)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("read from a proxy with no upstream")
	}
	conn.Close()
	// A delayed connection is released by the context when the proxy
	// stops.
	target, _ := echo(t)
	p = New(target.Addr().String())
	p.SetDelay(time.Hour)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- p.Serve(ctx, ln) }()
	conn, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("the delayed connection outlived the context")
	}
	ln.Close()
	if err := <-served; err != nil {
		t.Fatalf("Serve after the listener closed: %v", err)
	}
	// A listener that fails for another reason is reported.
	p.Close()
	if err := p.Serve(context.Background(), failingListener{}); err == nil {
		t.Fatal("a failing listener was not reported")
	}
}

type failingListener struct{ net.Listener }

func (failingListener) Accept() (net.Conn, error) { return nil, io.ErrUnexpectedEOF }
