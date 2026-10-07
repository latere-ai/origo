// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"testing"
	"time"

	"latere.ai/x/origo/test/stubs/source"
)

// fetchStatus sends the first request of a fetch of the stub's fixture
// in the background and answers the channel its status arrives on, -1
// when the request failed.
func fetchStatus(stub *source.Server) <-chan int {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(stub.CA())
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", stub.URL()+"/fixture.git/info/refs?service=git-upload-pack", nil)
		req.Header.Set("Authorization", "Bearer "+stub.Token())
		resp, err := client.Do(req)
		if err != nil {
			done <- -1
			return
		}
		_ = resp.Body.Close()
		done <- resp.StatusCode
	}()
	return done
}

// TestHoldSourceHoldsUntilReleased is the helper spec 021's import case
// pushes under: it holds the source stub through SourceControl, trusting
// SourceCA, so a git request reaches the stub and is not answered until
// the release the helper returns, which posts once however often it is
// called; a hold the case never releases ends with its test.
func TestHoldSourceHoldsUntilReleased(t *testing.T) {
	stub := source.New(t)
	s := &session{target: Target{SourceControl: stub.URL(), SourceCA: stub.CA()}, client: &http.Client{}}
	t.Run("released by the case", func(t *testing.T) {
		release := s.holdSource(t)
		done := fetchStatus(stub)
		deadline := time.Now().Add(10 * time.Second)
		for len(stub.Requests()) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("the request never reached the stub")
			}
			time.Sleep(10 * time.Millisecond)
		}
		select {
		case status := <-done:
			t.Fatalf("a request under the hold was answered %d", status)
		case <-time.After(300 * time.Millisecond):
		}
		release()
		if status := <-done; status != http.StatusOK {
			t.Fatalf("the released request: %d", status)
		}
		release()
	})
	t.Run("released with the test", func(t *testing.T) {
		s.holdSource(t)
	})
	select {
	case status := <-fetchStatus(stub):
		if status != http.StatusOK {
			t.Fatalf("a request after both holds ended: %d", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hold outlived the test that set it")
	}
}
