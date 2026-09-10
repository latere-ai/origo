// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package sshd

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
)

// The host key algorithms an installation may present (spec 024). A key
// outside the set is a start-up problem, not a key that is quietly
// skipped: an operator who generated the wrong kind of key finds out
// before a client ever sees it.
const (
	AlgoED25519 = ssh.KeyAlgoED25519
	AlgoECDSA   = ssh.KeyAlgoECDSA256
	AlgoRSA     = ssh.KeyAlgoRSA
)

// MinRSABits is the shortest RSA host key accepted.
const MinRSABits = 2048

// HostKeys is the ordered set of an installation's host keys: the same
// list on every node, read from files at start-up and never from the
// bucket.
//
// SSH negotiates one host key algorithm per connection and the server
// presents one key for it, so two keys of one algorithm cannot both be
// presented. The rule is explicit rather than inherited from the
// library: the first key of each algorithm is presented, and a second
// key of the same algorithm is announced only. Rotation is therefore
// append, wait, promote, drop, and clients that reconnect during the
// overlap learn the new key by themselves through
// hostkeys-00@openssh.com.
type HostKeys struct {
	// all is every key in the configured order, which is what is
	// announced and what a proof may be asked for.
	all []ssh.Signer
	// presented is the first key of each algorithm, in the same order.
	presented []ssh.Signer
}

// ParseHostKeys reads ORIGO_SSH_HOST_KEYS: an ordered list of paths to
// OpenSSH private key files. It collects every problem into one error,
// the way spec 002's start-up message names all of them.
func ParseHostKeys(paths []string) (*HostKeys, error) {
	if len(paths) == 0 {
		return nil, errors.New("no host key")
	}
	var problems []string
	h := &HostKeys{}
	seen := map[string]bool{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, path+": "+err.Error())
			continue
		}
		signer, err := ssh.ParsePrivateKey(raw)
		if err != nil {
			problems = append(problems, path+": "+err.Error())
			continue
		}
		if problem := checkHostKey(signer.PublicKey()); problem != "" {
			problems = append(problems, path+": "+problem)
			continue
		}
		h.all = append(h.all, signer)
		if algo := signer.PublicKey().Type(); !seen[algo] {
			seen[algo] = true
			h.presented = append(h.presented, signer)
		}
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	return h, nil
}

// checkHostKey reports why a key may not be a host key, or "".
func checkHostKey(pub ssh.PublicKey) string {
	switch pub.Type() {
	case AlgoED25519, AlgoECDSA:
		return ""
	case AlgoRSA:
		if bits := rsaBits(pub); bits < MinRSABits {
			return fmt.Sprintf("an RSA host key must be at least %d bits, this one is %d", MinRSABits, bits)
		}
		return ""
	default:
		return pub.Type() + " is not a host key algorithm; use ssh-ed25519, ecdsa-sha2-nistp256, or ssh-rsa"
	}
}

// rsaBits is the modulus size of an RSA public key, or 0 when the key
// does not expose one.
func rsaBits(pub ssh.PublicKey) int {
	ck, ok := pub.(ssh.CryptoPublicKey)
	if !ok {
		return 0
	}
	key, ok := ck.CryptoPublicKey().(*rsa.PublicKey)
	if !ok {
		return 0
	}
	return key.N.BitLen()
}

// Presented is the key of each algorithm the server offers, in the
// configured order.
func (h *HostKeys) Presented() []ssh.Signer { return h.presented }

// All is every configured key, which is what the announcement carries.
func (h *HostKeys) All() []ssh.Signer { return h.all }

// Fingerprints is every key's SHA-256 fingerprint, for the start-up log
// line and for an operator publishing them before a rotation.
func (h *HostKeys) Fingerprints() []string {
	out := make([]string, 0, len(h.all))
	for _, s := range h.all {
		out = append(out, ssh.FingerprintSHA256(s.PublicKey()))
	}
	return out
}

// The two OpenSSH extensions that carry a host key set over a
// connection. golang.org/x/crypto/ssh implements neither, so the wire
// format of OpenSSH's PROTOCOL document is written here.
const (
	hostKeysRequest = "hostkeys-00@openssh.com"
	hostKeysProve   = "hostkeys-prove-00@openssh.com"
)

// announcement is the payload of hostkeys-00@openssh.com: every
// configured public key, each as one SSH string. A client with
// UpdateHostKeys on writes the keys it does not hold into known_hosts by
// itself, which is what makes a rotation an overlap rather than a flag
// day.
func (h *HostKeys) announcement() []byte {
	var out []byte
	for _, s := range h.all {
		out = appendString(out, s.PublicKey().Marshal())
	}
	return out
}

// prove answers hostkeys-prove-00@openssh.com: for each key the client
// names, a signature over the extension name, the session identifier,
// and the key blob, in the configured key's own algorithm. A key the
// server does not hold is refused rather than skipped, so a client
// never reads a short answer as a proof of the keys it asked about.
func (h *HostKeys) prove(sessionID, payload []byte) ([]byte, error) {
	blobs, err := readStrings(payload)
	if err != nil {
		return nil, err
	}
	if len(blobs) == 0 {
		return nil, errors.New("hostkeys-prove: no key named")
	}
	var out []byte
	for _, blob := range blobs {
		signer := h.signerFor(blob)
		if signer == nil {
			return nil, errors.New("hostkeys-prove: this server holds no such key")
		}
		var data []byte
		data = appendString(data, []byte(hostKeysProve))
		data = appendString(data, sessionID)
		data = appendString(data, blob)
		sig, err := signer.Sign(rand.Reader, data)
		if err != nil {
			return nil, err
		}
		out = appendString(out, ssh.Marshal(sig))
	}
	return out, nil
}

// signerFor is the configured key whose blob is blob, or nil.
func (h *HostKeys) signerFor(blob []byte) ssh.Signer {
	for _, s := range h.all {
		if bytes.Equal(s.PublicKey().Marshal(), blob) {
			return s
		}
	}
	return nil
}

// appendString appends one SSH wire string: a big-endian length and the
// bytes.
func appendString(dst, s []byte) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(s)))
	return append(dst, s...)
}

// readStrings splits a payload of SSH wire strings, refusing a truncated
// or over-long one rather than reading past the buffer.
func readStrings(payload []byte) ([][]byte, error) {
	var out [][]byte
	for len(payload) > 0 {
		if len(payload) < 4 {
			return nil, errors.New("ssh: a truncated string in the payload")
		}
		n := binary.BigEndian.Uint32(payload)
		payload = payload[4:]
		if uint64(n) > uint64(len(payload)) {
			return nil, errors.New("ssh: a string longer than the payload")
		}
		out = append(out, payload[:n])
		payload = payload[n:]
	}
	return out, nil
}
