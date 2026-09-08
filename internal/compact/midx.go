// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package compact

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The multi-pack index framing git writes, from its format document: a
// 12 byte header (MIDX, a version, a hash version, the chunk count, the
// base file count, the pack count), then one 12 byte row per chunk plus
// a terminating row, each a four byte identifier and an eight byte
// offset. PNAM holds the pack index names, null-terminated.
const (
	midxHeaderSize = 12
	midxChunkSize  = 12
	midxVersion    = 1
)

var (
	midxSignature = []byte("MIDX")
	midxPNAM      = []byte("PNAM")
)

// packSet is the packs a repack left current: the multi-pack index it
// wrote names exactly them, the new ones and the large ones the
// geometric roll-up left alone, and not the packs it superseded, which
// stay on disk because the repack ran without -d. A repository with no
// pack at all has no multi-pack index and an empty set.
func packSet(dir string) ([]string, error) {
	packs := filepath.Join(dir, "objects", "pack")
	data, err := os.ReadFile(filepath.Join(packs, "multi-pack-index"))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if empty, err := noPacks(packs); err != nil || empty {
			return nil, err
		}
		return nil, errors.New("compact: the repack wrote no multi-pack index")
	}
	names, err := midxPacks(data)
	if err != nil {
		return nil, err
	}
	return names, nil
}

// noPacks reports whether the directory holds no .pack file.
func noPacks(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".pack") {
			return false, nil
		}
	}
	return true, nil
}

// midxPacks reads the PNAM chunk and answers the pack file names, one
// pack-<hash>.pack per index name it holds.
func midxPacks(data []byte) ([]string, error) {
	if len(data) < midxHeaderSize || !bytes.Equal(data[:4], midxSignature) {
		return nil, errors.New("compact: the multi-pack index has no MIDX signature")
	}
	if data[4] != midxVersion {
		return nil, fmt.Errorf("compact: multi-pack index version %d", data[4])
	}
	chunks := int(data[6])
	table := midxHeaderSize + (chunks+1)*midxChunkSize
	if chunks == 0 || len(data) < table {
		return nil, errors.New("compact: the multi-pack index has no chunk table")
	}
	for i := range chunks {
		row := midxHeaderSize + i*midxChunkSize
		if !bytes.Equal(data[row:row+4], midxPNAM) {
			continue
		}
		start := binary.BigEndian.Uint64(data[row+4 : row+12])
		end := binary.BigEndian.Uint64(data[row+midxChunkSize+4 : row+midxChunkSize+12])
		if start > end || end > uint64(len(data)) {
			return nil, errors.New("compact: the multi-pack index chunk table is out of range")
		}
		var names []string
		for name := range bytes.SplitSeq(data[start:end], []byte{0}) {
			base, ok := strings.CutSuffix(string(name), ".idx")
			if !ok {
				continue
			}
			names = append(names, base+".pack")
		}
		return names, nil
	}
	return nil, errors.New("compact: the multi-pack index names no packs")
}
