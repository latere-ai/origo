// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package slowproxy is the slow-bucket fault injector of spec 015: a
// TCP forwarder in front of MinIO that holds each connection's first
// bytes for a delay its control endpoint sets. The origo-stubs binary
// of spec 013 runs it when -slowproxy-target is set, with the data
// listener at -slowproxy-data, what ORIGO_S3_ENDPOINT on every node of
// the kind stack names, and the control endpoint at -slowproxy-listen,
// host port 30085 of the ports table. A delay above ORIGO_STORAGE_TIMEOUT
// makes every store call of a node time out, which is the slow case of
// that spec's cluster scenario.
package slowproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/httpjson"
	"latere.ai/x/pkg/wait"
)

// dialTimeout bounds one dial of the target.
const dialTimeout = 10 * time.Second

// Proxy forwards TCP to the target with a settable delay.
type Proxy struct {
	target string

	mu    sync.Mutex
	delay time.Duration
	conns map[net.Conn]struct{}
}

// New returns a proxy to target, host:port, with no delay.
func New(target string) *Proxy {
	return &Proxy{target: target, conns: map[net.Conn]struct{}{}}
}

// Target is what the proxy forwards to.
func (p *Proxy) Target() string { return p.target }

// Delay is the current delay.
func (p *Proxy) Delay() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.delay
}

// SetDelay sets the delay every new connection's first bytes are held
// for and closes every forwarded connection, so a node's pooled
// connections are not exempt: its next request opens a new one and
// meets the delay.
func (p *Proxy) SetDelay(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.delay = max(d, 0)
	for c := range p.conns {
		_ = c.Close()
	}
}

// state is the body of the control endpoint.
type state struct {
	Target string `json:"target"`
	Delay  string `json:"delay"`
}

func (p *Proxy) state() state {
	return state{Target: p.target, Delay: p.Delay().String()}
}

// Handler is the control endpoint: GET / reports the target and the
// delay; PUT /delay with {"delay": "<duration>"} sets it and reports
// the state; DELETE /delay clears it. A malformed body is 400.
func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		httpjson.Write(w, http.StatusOK, p.state())
	})
	mux.HandleFunc("PUT /delay", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Delay string `json:"delay"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body); err != nil {
			http.Error(w, "body: "+err.Error(), http.StatusBadRequest)
			return
		}
		d, err := time.ParseDuration(body.Delay)
		if err != nil || d < 0 {
			http.Error(w, "delay: a non-negative duration such as 15s", http.StatusBadRequest)
			return
		}
		p.SetDelay(d)
		httpjson.Write(w, http.StatusOK, p.state())
	})
	mux.HandleFunc("DELETE /delay", func(w http.ResponseWriter, _ *http.Request) {
		p.SetDelay(0)
		httpjson.Write(w, http.StatusOK, p.state())
	})
	return mux
}

// Serve forwards every connection accepted on ln to the target until
// the listener closes; ctx bounds each dial of the target and each
// delay.
func (p *Proxy) Serve(ctx context.Context, ln net.Listener) error {
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

// forward holds the client's first bytes for the delay, dials the
// target, and copies both ways until either side closes.
func (p *Proxy) forward(ctx context.Context, client net.Conn) {
	defer func() { _ = client.Close() }()
	p.track(client, true)
	defer p.track(client, false)
	if d := p.Delay(); d > 0 {
		if err := wait.Sleep(ctx, d); err != nil {
			return
		}
	}
	var dialer net.Dialer
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	upstream, err := dialer.DialContext(dialCtx, "tcp", p.target)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()
	p.track(upstream, true)
	defer p.track(upstream, false)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

func (p *Proxy) track(c net.Conn, add bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if add {
		p.conns[c] = struct{}{}
	} else {
		delete(p.conns, c)
	}
}

// Close ends every forwarded connection.
func (p *Proxy) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		_ = c.Close()
	}
}

// controlClient reaches a control endpoint from a test; a plain
// transport, because the caller is a test process, not a service.
var controlClient = &http.Client{Transport: &http.Transport{}, Timeout: 10 * time.Second}

// Set sets the delay of the proxy whose control endpoint is at url,
// the way a cluster test drives the stack's proxy through host port
// 30085 of the ports table.
func Set(ctx context.Context, url string, d time.Duration) error {
	body := fmt.Sprintf(`{"delay":%q}`, d.String())
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, strings.TrimRight(url, "/")+"/delay", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := controlClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("slowproxy: %s: %d %s", url, resp.StatusCode, strings.TrimSpace(string(text)))
	}
	return nil
}
