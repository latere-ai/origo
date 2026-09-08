// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// slowproxy is the shell of spec 015's slow proxy: a TCP forwarder to
// the target with a control endpoint that reports the delay, which is
// 0 and cannot be set here. Spec 015 builds test/stubs/slowproxy with
// the fault behaviour and replaces this shell; the flags, the start
// rule (only with -slowproxy-target), and the two listeners are fixed
// by spec 013 so the overlay row of spec 015 is a change to the
// Deployment's arguments and nothing else.
type slowproxy struct {
	target   string
	dataAddr string
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
}

func newSlowproxy(target string) *slowproxy {
	return &slowproxy{target: target, conns: map[net.Conn]struct{}{}}
}

// control serves GET / with the target and the delay.
func (p *slowproxy) control() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"target": p.target, "delay": "0s"})
	})
	return mux
}

// serve forwards every accepted connection to the target until the
// listener closes; ctx bounds each dial of the target.
func (p *slowproxy) serve(ctx context.Context, ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go p.forward(ctx, conn)
	}
}

func (p *slowproxy) forward(ctx context.Context, client net.Conn) {
	defer func() { _ = client.Close() }()
	var d net.Dialer
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	upstream, err := d.DialContext(dialCtx, "tcp", p.target)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()
	p.track(client, true)
	p.track(upstream, true)
	defer p.track(client, false)
	defer p.track(upstream, false)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

func (p *slowproxy) track(c net.Conn, add bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if add {
		p.conns[c] = struct{}{}
	} else {
		delete(p.conns, c)
	}
}

// close ends every forwarded connection.
func (p *slowproxy) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		_ = c.Close()
	}
}
