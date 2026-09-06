// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// pkt-line framing, the one part of git's wire protocol this package
// reads for itself: a four hex digit length including itself, then the
// payload; "0000" is a flush and "0001" a delimiter. The largest line
// git writes is 65520 bytes.
const (
	maxPktLine = 65520
	pktFlush   = -1
	pktDelim   = -2
)

// readPkt reads one pkt-line. It returns the payload and 0, or nil and
// pktFlush / pktDelim for the control packets.
func readPkt(br *bufio.Reader) ([]byte, int, error) {
	var head [4]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		return nil, 0, err
	}
	var n [2]byte
	if _, err := hex.Decode(n[:], head[:]); err != nil {
		return nil, 0, fmt.Errorf("pkt-line: bad length %q", head)
	}
	size := int(n[0])<<8 | int(n[1])
	switch {
	case size == 0:
		return nil, pktFlush, nil
	case size == 1:
		return nil, pktDelim, nil
	case size < 4:
		return nil, 0, fmt.Errorf("pkt-line: length %d", size)
	case size > maxPktLine:
		return nil, 0, fmt.Errorf("pkt-line: length %d exceeds %d", size, maxPktLine)
	}
	payload := make([]byte, size-4)
	if _, err := io.ReadFull(br, payload); err != nil {
		return nil, 0, fmt.Errorf("pkt-line: %w", err)
	}
	return payload, 0, nil
}

// writePkt frames one payload.
func writePkt(w io.Writer, payload string) error {
	if len(payload)+4 > maxPktLine {
		return errors.New("pkt-line: payload too long")
	}
	_, err := fmt.Fprintf(w, "%04x%s", len(payload)+4, payload)
	return err
}

// flushPkt writes a flush packet.
func flushPkt(w io.Writer) error {
	_, err := io.WriteString(w, "0000")
	return err
}
