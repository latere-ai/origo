// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/latere-ai/origo/internal/wal"
)

// receiveRequest is a parsed git-receive-pack body: the commands the
// client sent, the capabilities it asked for, its push options, and
// where the packfile starts in the spooled body.
type receiveRequest struct {
	Commands     []wal.RefUpdate
	Capabilities []string
	Options      []string
	// PackOffset is where the pack begins in the spool; PackSize is its
	// length, zero for a push that carries no objects.
	PackOffset int64
	PackSize   int64
}

// maxCommands bounds one push's reference updates.
const maxCommands = 100000

// spoolBody copies the request body to a file under dir, inflating a
// gzip body as git sends for large pushes, and returns the file open at
// its start. The caller removes it. Spooling every body, however small,
// keeps one path: the pack has to be re-read for the entry after git
// consumed it, and a file is re-readable where a socket is not.
func spoolBody(r *http.Request, dir string) (*os.File, error) {
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, fmt.Errorf("gzip body: %w", err)
		}
		defer func() { _ = gz.Close() }()
		body = gz
	}
	f, err := os.CreateTemp(dir, "receive-*.body")
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	return f, nil
}

// parseReceive reads the command section of a receive-pack body off r,
// which is positioned at its start, and reports where the pack begins.
// The format: pkt-lines "<old> <new> <ref>", the first one followed by
// NUL and the capability list, then a flush; with push-options among
// the capabilities, option pkt-lines and a second flush; then the pack.
func parseReceive(r io.ReadSeeker) (*receiveRequest, error) {
	br := bufio.NewReader(r)
	req := &receiveRequest{}
	for {
		line, kind, err := readPkt(br)
		if err != nil {
			return nil, fmt.Errorf("receive-pack commands: %w", err)
		}
		if kind == pktFlush {
			break
		}
		if kind == pktDelim {
			return nil, errors.New("receive-pack commands: unexpected delimiter")
		}
		if len(req.Commands) == 0 {
			if cmd, caps, ok := bytes.Cut(line, []byte{0}); ok {
				line = cmd
				req.Capabilities = strings.Fields(string(caps))
			}
		}
		update, err := parseCommand(line)
		if err != nil {
			return nil, err
		}
		req.Commands = append(req.Commands, update)
		if len(req.Commands) > maxCommands {
			return nil, fmt.Errorf("receive-pack: more than %d commands", maxCommands)
		}
	}
	hasOptions := false
	for _, c := range req.Capabilities {
		if c == "push-options" {
			hasOptions = true
		}
	}
	if hasOptions && len(req.Commands) > 0 {
		for {
			line, kind, err := readPkt(br)
			if err != nil {
				return nil, fmt.Errorf("receive-pack options: %w", err)
			}
			if kind == pktFlush {
				break
			}
			if kind != 0 {
				return nil, errors.New("receive-pack options: unexpected delimiter")
			}
			req.Options = append(req.Options, strings.TrimSuffix(string(line), "\n"))
			if len(req.Options) > 1000 {
				return nil, errors.New("receive-pack: more than 1000 push options")
			}
		}
	}
	// Where the pack starts is where the reader stopped, minus what the
	// buffer read ahead.
	pos, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	req.PackOffset = pos - int64(br.Buffered())
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	req.PackSize = end - req.PackOffset
	if req.PackSize > 0 && req.PackSize < 12 {
		return nil, errors.New("receive-pack: truncated pack")
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return req, nil
}

// parseCommand reads "<old> <new> <ref>".
func parseCommand(line []byte) (wal.RefUpdate, error) {
	fields := strings.Fields(strings.TrimSuffix(string(line), "\n"))
	if len(fields) != 3 {
		return wal.RefUpdate{}, fmt.Errorf("receive-pack: malformed command %q", line)
	}
	u := wal.RefUpdate{Old: fields[0], New: fields[1], Ref: fields[2]}
	if err := wal.ValidateTransaction([]wal.RefUpdate{u}); err != nil {
		return wal.RefUpdate{}, err
	}
	if u.Ref == "HEAD" {
		return wal.RefUpdate{}, errors.New("receive-pack: HEAD cannot be pushed")
	}
	return u, nil
}
