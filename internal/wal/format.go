// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Version is the format version every header and index object carries.
const Version = 1

// Kind is what an entry records.
type Kind string

// The entry kinds spec 004 defines.
const (
	KindPush    Kind = "push"
	KindCompact Kind = "compact"
	KindDelete  Kind = "delete"
)

// ZeroSHA is the object id of an absent reference in a transaction.
const ZeroSHA = "0000000000000000000000000000000000000000"

// Limits on what a parser accepts from bytes it did not write.
const (
	maxHeaderLine = 4 << 10
	maxRefsLine   = 64 << 20
	maxIndexBytes = 64 << 20
	maxSeq        = 999999999999
)

// Header is the first line of an entry.
type Header struct {
	V          int       `json:"v"`
	Kind       Kind      `json:"kind"`
	Seq        uint64    `json:"seq"`
	At         time.Time `json:"at"`
	Subject    string    `json:"subject"`
	Actor      string    `json:"actor"`
	PackBytes  int64     `json:"pack_bytes"`
	PackSHA256 string    `json:"pack_sha256"`
	// PushOptions are the options the client sent with a push, such as
	// origo.event=off, recorded for spec 008 to read.
	PushOptions []string `json:"push_options,omitempty"`
}

// RefUpdate is one line of a reference transaction. Old and New are
// object ids, ZeroSHA for a create or a delete; for HEAD they are the
// symbolic target in the form "ref: refs/heads/main".
type RefUpdate struct {
	Ref string `json:"ref"`
	Old string `json:"old"`
	New string `json:"new"`
}

// IndexEntry is one entry named by an index object.
type IndexEntry struct {
	Seq  uint64 `json:"seq"`
	Key  string `json:"key"`
	Kind Kind   `json:"kind"`
	// PackBytes is the entry's pack_bytes. size_bytes is the bytes of
	// the listed packs plus the sum of this field over the rows, and
	// compaction (spec 006) reads the sum for its byte threshold
	// without opening an entry. Absent on a row with no pack and on an
	// index object written before the field existed.
	PackBytes  int64  `json:"pack_bytes,omitempty"`
	PackSHA256 string `json:"pack_sha256"`
}

// EntriesBytes is the sum of pack_bytes over the entries since the last
// compaction, the byte threshold of spec 006.
func (ix *Index) EntriesBytes() int64 {
	var n int64
	for _, e := range ix.Entries {
		n += e.PackBytes
	}
	return n
}

// Index is the complete state of a repository after entry Seq.
type Index struct {
	V                int               `json:"v"`
	Seq              uint64            `json:"seq"`
	Entry            string            `json:"entry"`
	Refs             map[string]string `json:"refs"`
	Entries          []IndexEntry      `json:"entries"`
	Packs            []string          `json:"packs"`
	CompactedThrough uint64            `json:"compacted_through"`
	SizeBytes        int64             `json:"size_bytes"`
	DeletedAt        *time.Time        `json:"deleted_at"`
	// PushedAt is the at of the newest push entry: a push commit sets
	// it to its own entry's at, every other commit copies it forward,
	// and index 0 holds null. An index object written before the field
	// existed has none and reads as null.
	PushedAt *time.Time `json:"pushed_at"`
}

// Clone returns a deep copy so a caller mutates its own map.
func (ix *Index) Clone() *Index {
	c := *ix
	c.Refs = make(map[string]string, len(ix.Refs))
	maps.Copy(c.Refs, ix.Refs)
	c.Entries = append([]IndexEntry(nil), ix.Entries...)
	c.Packs = append([]string(nil), ix.Packs...)
	if ix.DeletedAt != nil {
		t := *ix.DeletedAt
		c.DeletedAt = &t
	}
	if ix.PushedAt != nil {
		t := *ix.PushedAt
		c.PushedAt = &t
	}
	return &c
}

// DefaultBranch reads the branch HEAD points at, or "" when HEAD is
// detached or absent.
func (ix *Index) DefaultBranch() string {
	return strings.TrimPrefix(strings.TrimPrefix(ix.Refs["HEAD"], "ref: "), "refs/heads/")
}

// SeqString renders a sequence as the zero-padded 12 digit decimal the
// keys use, so they sort in sequence order under a listing.
func SeqString(seq uint64) string { return fmt.Sprintf("%012d", seq) }

// IndexKey is the key of the index object for seq, relative to the
// repository prefix.
func IndexKey(seq uint64) string { return "index/" + SeqString(seq) }

// EntryKey is the key of an entry, relative to the repository prefix.
func EntryKey(seq uint64, nonce string) string {
	return "wal/" + SeqString(seq) + "." + nonce + ".entry"
}

// LatestKey is the hint object's key.
const LatestKey = "index/latest"

// PackFile maps a pack object of the log to the file git reads under
// objects/pack: packs/<hash>.pack is pack-<hash>.pack, and packs/<hash>.idx
// is pack-<hash>.idx. The hash is the one git put in the file name, so a
// pack keeps one name everywhere; every spec that moves a pack uses this
// mapping.
func PackFile(key string) string { return "pack-" + strings.TrimPrefix(key, "packs/") }

// PackKey is PackFile the other way: the log key of a file git wrote
// under objects/pack. A compaction (spec 006) uploads the packs its
// repack produced under the key of the name git gave each file.
func PackKey(file string) string { return "packs/" + strings.TrimPrefix(file, "pack-") }

var (
	entryKeyRe = regexp.MustCompile(`^wal/(\d{12})\.([0-9a-f]{16})\.entry$`)
	indexKeyRe = regexp.MustCompile(`^index/(\d{12})$`)
	shaRe      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	packKeyRe  = regexp.MustCompile(`^packs/[0-9a-f]{40,64}\.pack$`)
)

// ParseEntryKey reads the sequence and nonce out of an entry key.
func ParseEntryKey(key string) (seq uint64, nonce string, ok bool) {
	m := entryKeyRe.FindStringSubmatch(key)
	if m == nil {
		return 0, "", false
	}
	seq, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, "", false
	}
	return seq, m[2], true
}

// ParseIndexKey reads the sequence out of an index key.
func ParseIndexKey(key string) (uint64, bool) {
	m := indexKeyRe.FindStringSubmatch(key)
	if m == nil {
		return 0, false
	}
	seq, err := strconv.ParseUint(m[1], 10, 64)
	return seq, err == nil
}

// ValidRefName reports whether name is a reference git accepts: HEAD, or
// a fully qualified name under refs/ that passes git's own rules.
func ValidRefName(name string) bool {
	if name == "HEAD" {
		return true
	}
	if !strings.HasPrefix(name, "refs/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock") {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.Contains(name, "//") {
		return false
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7f || strings.ContainsRune(" ~^:?*[\\", c) {
			return false
		}
	}
	for comp := range strings.SplitSeq(name, "/") {
		if comp == "" || strings.HasPrefix(comp, ".") || comp == "@" {
			return false
		}
	}
	return true
}

func validSHA(s string) bool { return shaRe.MatchString(s) }

// validRefValue accepts an object id, or a symbolic target for HEAD.
func validRefValue(ref, v string) bool {
	if validSHA(v) {
		return true
	}
	if ref == "HEAD" && strings.HasPrefix(v, "ref: ") {
		target := strings.TrimPrefix(v, "ref: ")
		return target != "HEAD" && ValidRefName(target)
	}
	return false
}

// ParseHeader validates one header line.
func ParseHeader(line []byte) (Header, error) {
	var h Header
	if len(line) > maxHeaderLine {
		return h, errors.New("wal: header exceeds 4 KiB")
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		return h, fmt.Errorf("wal: header: %w", err)
	}
	if h.V != Version {
		return h, fmt.Errorf("wal: header version %d, want %d", h.V, Version)
	}
	switch h.Kind {
	case KindPush, KindCompact, KindDelete:
	default:
		return h, fmt.Errorf("wal: header kind %q", h.Kind)
	}
	if h.Seq == 0 || h.Seq > maxSeq {
		return h, fmt.Errorf("wal: header seq %d out of range", h.Seq)
	}
	if h.PackBytes < 0 {
		return h, errors.New("wal: header pack_bytes negative")
	}
	if h.PackBytes > 0 && !hexRe.MatchString(h.PackSHA256) {
		return h, errors.New("wal: header pack_sha256 is not a sha256")
	}
	if h.At.IsZero() {
		return h, errors.New("wal: header at is missing")
	}
	return h, nil
}

var hexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseTransaction validates one reference transaction line.
func ParseTransaction(line []byte) ([]RefUpdate, error) {
	if len(line) > maxRefsLine {
		return nil, errors.New("wal: transaction exceeds 64 MiB")
	}
	var refs []RefUpdate
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&refs); err != nil {
		return nil, fmt.Errorf("wal: transaction: %w", err)
	}
	return refs, ValidateTransaction(refs)
}

// ValidateTransaction checks every update names a valid reference once
// with valid values.
func ValidateTransaction(refs []RefUpdate) error {
	seen := make(map[string]bool, len(refs))
	for _, u := range refs {
		if !ValidRefName(u.Ref) {
			return fmt.Errorf("wal: transaction: invalid reference %q", u.Ref)
		}
		if seen[u.Ref] {
			return fmt.Errorf("wal: transaction: reference %q appears twice", u.Ref)
		}
		seen[u.Ref] = true
		if !validRefValue(u.Ref, u.Old) || !validRefValue(u.Ref, u.New) {
			return fmt.Errorf("wal: transaction: invalid value on %q", u.Ref)
		}
		if u.Old == u.New {
			return fmt.Errorf("wal: transaction: %q does not change", u.Ref)
		}
	}
	return nil
}

// ParseIndex validates an index object.
func ParseIndex(data []byte) (*Index, error) {
	if len(data) > maxIndexBytes {
		return nil, errors.New("wal: index exceeds 64 MiB")
	}
	var ix Index
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ix); err != nil {
		return nil, fmt.Errorf("wal: index: %w", err)
	}
	if ix.V != Version {
		return nil, fmt.Errorf("wal: index version %d, want %d", ix.V, Version)
	}
	if ix.Seq > maxSeq {
		return nil, fmt.Errorf("wal: index seq %d out of range", ix.Seq)
	}
	if ix.Seq == 0 && ix.Entry != "" {
		return nil, errors.New("wal: index 0 names an entry")
	}
	if ix.Seq > 0 {
		seq, _, ok := ParseEntryKey(ix.Entry)
		if !ok || seq != ix.Seq {
			return nil, fmt.Errorf("wal: index %d names entry %q", ix.Seq, ix.Entry)
		}
	}
	if ix.Refs == nil {
		ix.Refs = map[string]string{}
	}
	for ref, v := range ix.Refs {
		if !ValidRefName(ref) || !validRefValue(ref, v) {
			return nil, fmt.Errorf("wal: index: invalid reference %q", ref)
		}
	}
	if ix.CompactedThrough > ix.Seq {
		return nil, errors.New("wal: index compacted_through is above seq")
	}
	var last uint64
	for i, e := range ix.Entries {
		seq, _, ok := ParseEntryKey(e.Key)
		if !ok || seq != e.Seq || e.Seq <= ix.CompactedThrough || e.Seq > ix.Seq || (i > 0 && e.Seq <= last) {
			return nil, fmt.Errorf("wal: index: entry %q out of order", e.Key)
		}
		last = e.Seq
		switch e.Kind {
		case KindPush, KindCompact, KindDelete:
		default:
			return nil, fmt.Errorf("wal: index: entry kind %q", e.Kind)
		}
		if e.PackBytes < 0 {
			return nil, fmt.Errorf("wal: index: entry %q pack_bytes negative", e.Key)
		}
	}
	if ix.Seq > 0 && (len(ix.Entries) == 0 || ix.Entries[len(ix.Entries)-1].Key != ix.Entry) {
		return nil, errors.New("wal: index does not list its own entry last")
	}
	for _, p := range ix.Packs {
		if !packKeyRe.MatchString(p) {
			return nil, fmt.Errorf("wal: index: pack %q", p)
		}
	}
	if ix.SizeBytes < 0 {
		return nil, errors.New("wal: index size_bytes negative")
	}
	return &ix, nil
}

// EncodeIndex renders an index object.
func EncodeIndex(ix *Index) ([]byte, error) {
	if ix.Refs == nil {
		ix.Refs = map[string]string{}
	}
	if ix.Entries == nil {
		ix.Entries = []IndexEntry{}
	}
	if ix.Packs == nil {
		ix.Packs = []string{}
	}
	return json.Marshal(ix)
}

// EncodeEntryHead renders the two lines that precede the pack.
func EncodeEntryHead(h Header, refs []RefUpdate) ([]byte, error) {
	if refs == nil {
		refs = []RefUpdate{}
	}
	hb, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	rb, err := json.Marshal(refs)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(hb)+len(rb)+2)
	out = append(out, hb...)
	out = append(out, '\n')
	out = append(out, rb...)
	out = append(out, '\n')
	return out, nil
}

// ReadEntryHead parses the header and the transaction off r and returns
// the reader positioned at the pack.
func ReadEntryHead(r io.Reader) (Header, []RefUpdate, io.Reader, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	line, err := readLine(br, maxHeaderLine)
	if err != nil {
		return Header{}, nil, nil, fmt.Errorf("wal: entry header: %w", err)
	}
	h, err := ParseHeader(line)
	if err != nil {
		return Header{}, nil, nil, err
	}
	line, err = readLine(br, maxRefsLine)
	if err != nil {
		return Header{}, nil, nil, fmt.Errorf("wal: entry transaction: %w", err)
	}
	refs, err := ParseTransaction(line)
	if err != nil {
		return Header{}, nil, nil, err
	}
	return h, refs, br, nil
}

// readLine reads up to and excluding a newline, refusing a line longer
// than limit rather than buffering it.
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	for {
		chunk, err := br.ReadSlice('\n')
		out = append(out, chunk...)
		if len(out) > limit+1 {
			return nil, fmt.Errorf("line exceeds %d bytes", limit)
		}
		if err == nil {
			return out[:len(out)-1], nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}
