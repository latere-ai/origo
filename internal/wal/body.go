// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"bytes"
	"crypto/md5" //nolint:gosec // Content-MD5
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
)

// concatBody joins a head held in memory and a pack body into one Body,
// hashing the combination once. The pack is read once here and once per
// upload attempt.
func concatBody(head []byte, pack Body) (Body, error) {
	h := sha256.New()
	m := md5.New() //nolint:gosec // Content-MD5
	h.Write(head)
	m.Write(head)
	size := int64(len(head))
	if pack.Size > 0 {
		rc, err := pack.Open()
		if err != nil {
			return Body{}, err
		}
		n, err := io.Copy(io.MultiWriter(h, m), rc)
		_ = rc.Close()
		if err != nil {
			return Body{}, err
		}
		size += n
	}
	return Body{
		Open: func() (io.ReadCloser, error) {
			if pack.Size == 0 {
				return io.NopCloser(bytes.NewReader(head)), nil
			}
			rc, err := pack.Open()
			if err != nil {
				return nil, err
			}
			return &concatReader{Reader: io.MultiReader(bytes.NewReader(head), rc), closer: rc}, nil
		},
		Size:   size,
		SHA256: hex.EncodeToString(h.Sum(nil)),
		MD5:    base64.StdEncoding.EncodeToString(m.Sum(nil)),
	}, nil
}

type concatReader struct {
	io.Reader
	closer io.Closer
}

func (c *concatReader) Close() error { return c.closer.Close() }
