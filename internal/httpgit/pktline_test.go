// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestPktLineRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := writePkt(&buf, "# service=git-upload-pack\n"); err != nil {
		t.Fatal(err)
	}
	_ = flushPkt(&buf)
	buf.WriteString("0001")
	if !strings.HasPrefix(buf.String(), "001e# service=git-upload-pack\n0000") {
		t.Fatalf("framed = %q", buf.String())
	}
	br := bufio.NewReader(&buf)
	line, kind, err := readPkt(br)
	if err != nil || kind != 0 || string(line) != "# service=git-upload-pack\n" {
		t.Fatalf("line = %q %d %v", line, kind, err)
	}
	if _, kind, err := readPkt(br); err != nil || kind != pktFlush {
		t.Fatalf("flush = %d %v", kind, err)
	}
	if _, kind, err := readPkt(br); err != nil || kind != pktDelim {
		t.Fatalf("delim = %d %v", kind, err)
	}
	if _, _, err := readPkt(br); err == nil {
		t.Fatal("end of stream accepted")
	}
	for _, bad := range []string{"zzzz", "0003", "0005", "fff0abc", "ffff"} {
		if _, _, err := readPkt(bufio.NewReader(strings.NewReader(bad))); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := writePkt(&buf, strings.Repeat("x", maxPktLine)); err == nil {
		t.Fatal("oversized payload accepted")
	}
}

func FuzzReadPkt(f *testing.F) {
	f.Add([]byte("0006ab0000"))
	f.Add([]byte("0001"))
	f.Add([]byte("00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		br := bufio.NewReader(bytes.NewReader(data))
		for range 64 {
			line, kind, err := readPkt(br)
			if err != nil {
				return
			}
			if kind == 0 && len(line) > maxPktLine-4 {
				t.Fatalf("line of %d bytes accepted", len(line))
			}
		}
	})
}

func FuzzParseReceive(f *testing.F) {
	sha1 := strings.Repeat("1", 40)
	zero := strings.Repeat("0", 40)
	var buf bytes.Buffer
	_ = writePkt(&buf, zero+" "+sha1+" refs/heads/main\x00report-status-v2 push-options\n")
	_ = flushPkt(&buf)
	_ = writePkt(&buf, "origo.event=off\n")
	_ = flushPkt(&buf)
	buf.WriteString("PACK\x00\x00\x00\x02\x00\x00\x00\x00")
	f.Add(buf.Bytes())
	f.Add([]byte("0000"))
	f.Fuzz(func(t *testing.T, data []byte) {
		req, err := parseReceive(bytes.NewReader(data))
		if err != nil {
			return
		}
		if req.PackOffset < 0 || req.PackOffset+req.PackSize != int64(len(data)) {
			t.Fatalf("pack offset %d size %d of %d", req.PackOffset, req.PackSize, len(data))
		}
	})
}
