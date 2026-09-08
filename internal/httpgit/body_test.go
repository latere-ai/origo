// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func receiveBody(t *testing.T, caps string, options []string, pack string, refs ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	for i, r := range refs {
		line := r + "\n"
		if i == 0 && caps != "" {
			line = r + "\x00" + caps + "\n"
		}
		if err := writePkt(&buf, line); err != nil {
			t.Fatal(err)
		}
	}
	_ = flushPkt(&buf)
	if options != nil {
		for _, o := range options {
			_ = writePkt(&buf, o+"\n")
		}
		_ = flushPkt(&buf)
	}
	buf.WriteString(pack)
	return buf.Bytes()
}

func TestParseReceiveFindsCommandsOptionsAndThePack(t *testing.T) {
	zero := strings.Repeat("0", 40)
	one := strings.Repeat("1", 40)
	two := strings.Repeat("2", 40)
	pack := "PACK\x00\x00\x00\x02\x00\x00\x00\x00rest"
	body := receiveBody(t, "report-status-v2 atomic push-options", []string{"origo.event=off", "x=y"}, pack,
		zero+" "+one+" refs/heads/main", one+" "+two+" refs/heads/dev")
	req, err := parseReceive(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Commands) != 2 || req.Commands[1].Ref != "refs/heads/dev" || req.Commands[0].Old != zero {
		t.Fatalf("commands = %+v", req.Commands)
	}
	if strings.Join(req.Capabilities, ",") != "report-status-v2,atomic,push-options" || strings.Join(req.Options, ",") != "origo.event=off,x=y" {
		t.Fatalf("caps %v options %v", req.Capabilities, req.Options)
	}
	if string(body[req.PackOffset:req.PackOffset+req.PackSize]) != pack {
		t.Fatalf("pack at %d+%d: %q", req.PackOffset, req.PackSize, body[req.PackOffset:])
	}
	// No options block without the capability; no pack on a delete.
	req, err = parseReceive(bytes.NewReader(receiveBody(t, "report-status", nil, "", one+" "+zero+" refs/heads/main")))
	if err != nil || req.PackSize != 0 || len(req.Options) != 0 {
		t.Fatalf("delete: %+v, %v", req, err)
	}
	// A flush alone is an empty push.
	req, err = parseReceive(bytes.NewReader([]byte("0000")))
	if err != nil || len(req.Commands) != 0 || req.PackSize != 0 {
		t.Fatalf("empty: %+v, %v", req, err)
	}
	for name, body := range map[string][]byte{
		"truncated":     []byte("00"),
		"bad command":   receiveBody(t, "", nil, "", "not a command"),
		"bad ref":       receiveBody(t, "", nil, "", zero+" "+one+" main"),
		"HEAD":          receiveBody(t, "", nil, "", "ref: refs/heads/a ref: refs/heads/b HEAD"),
		"delimiter":     []byte("0001"),
		"options cut":   receiveBody(t, "push-options", nil, "", zero+" "+one+" refs/heads/main"),
		"options delim": append(receiveBody(t, "push-options", nil, "", zero+" "+one+" refs/heads/main"), []byte("0001")...),
		"short pack":    receiveBody(t, "", nil, "PACK", zero+" "+one+" refs/heads/main"),
		"too many": func() []byte {
			refs := make([]string, maxCommands+1)
			for i := range refs {
				refs[i] = zero + " " + one + " refs/heads/b" + itoa(i)
			}
			return receiveBody(t, "", nil, "", refs...)
		}(),
	} {
		if _, err := parseReceive(bytes.NewReader(body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	many := make([]string, 1001)
	for i := range many {
		many[i] = "o=" + itoa(i)
	}
	if _, err := parseReceive(bytes.NewReader(receiveBody(t, "push-options", many, "", zero+" "+one+" refs/heads/main"))); err == nil {
		t.Error("1001 options accepted")
	}
	// Seek failures surface.
	if _, err := parseReceive(&brokenSeeker{Reader: bytes.NewReader([]byte("0000"))}); err == nil {
		t.Error("unseekable body accepted")
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

type brokenSeeker struct {
	io.Reader
}

func (brokenSeeker) Seek(int64, int) (int64, error) { return 0, errors.New("no seek") }

func TestSpoolBodyHandlesGzipAndFailures(t *testing.T) {
	dir := t.TempDir()
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte("0000"))
	_ = w.Close()
	req := httptest.NewRequest("POST", "/", &gz)
	req.Header.Set("Content-Encoding", "gzip")
	f, err := spoolBody(req, dir)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(f)
	_ = f.Close()
	if string(data) != "0000" {
		t.Fatalf("spooled %q", data)
	}
	req = httptest.NewRequest("POST", "/", strings.NewReader("not gzip"))
	req.Header.Set("Content-Encoding", "gzip")
	if _, err := spoolBody(req, dir); err == nil {
		t.Fatal("bad gzip accepted")
	}
	req = httptest.NewRequest("POST", "/", brokenSeeker{Reader: &failingReader{}})
	if _, err := spoolBody(req, dir); err == nil {
		t.Fatal("failed read accepted")
	}
	if _, err := spoolBody(httptest.NewRequest("POST", "/", strings.NewReader("x")), dir+"/missing"); err == nil {
		t.Fatal("missing spool directory accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("%d spool files left, want the one still open", len(entries))
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestHookChannelCarriesUpdatesAndVerdict(t *testing.T) {
	dir := t.TempDir()
	ch, err := newHookChannel(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.close()
	zero := strings.Repeat("0", 40)
	one := strings.Repeat("1", 40)
	// A writer plays the hook the way the script does: it opens the
	// updates FIFO for writing, writes the transaction and the
	// terminator, closes, then opens the verdict FIFO for reading and
	// reads one line. Neither open waits, because the node holds both
	// ends of each FIFO.
	verdict := make(chan string, 1)
	go func() {
		w, err := os.OpenFile(filepath.Join(ch.dir, "updates"), os.O_WRONLY, 0)
		if err != nil {
			verdict <- "open updates: " + err.Error()
			return
		}
		_, _ = w.WriteString(zero + " " + one + " refs/heads/main\n" + updatesEnd + "\n")
		_ = w.Close()
		r, err := os.OpenFile(filepath.Join(ch.dir, "verdict"), os.O_RDONLY, 0)
		if err != nil {
			verdict <- "open verdict: " + err.Error()
			return
		}
		line, _ := bufio.NewReader(r).ReadString('\n')
		_ = r.Close()
		verdict <- line
	}()
	refs, quarantine, ok, err := ch.readUpdates()
	if err != nil || !ok || quarantine != "" || len(refs) != 1 || refs[0].New != one {
		t.Fatalf("updates: %+v %v %v", refs, ok, err)
	}
	if err := ch.writeVerdict("reject non_fast_forward: fetch\nfirst"); err != nil {
		t.Fatal(err)
	}
	if got := <-verdict; got != "reject non_fast_forward: fetch first\n" {
		t.Fatalf("verdict = %q", got)
	}
	// Released before the hook ever wrote: no updates, and the release
	// itself does not wait for the reader.
	ch2, _ := newHookChannel(dir)
	defer ch2.close()
	if err := ch2.release(); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := ch2.readUpdates(); ok || err != nil {
		t.Fatalf("released channel: ok %v, %v", ok, err)
	}
	// A malformed line from the hook is an error.
	ch3, _ := newHookChannel(dir)
	defer ch3.close()
	_, _ = ch3.updates.WriteString("garbage\n" + updatesEnd + "\n")
	if _, _, _, err := ch3.readUpdates(); err == nil {
		t.Fatal("garbage accepted")
	}
	// The quarantine line comes first and is not a command; the
	// transaction follows it.
	ch4, _ := newHookChannel(dir)
	defer ch4.close()
	_, _ = ch4.updates.WriteString("quarantine /tmp/q\n" + strings.Repeat("0", 40) + " " + strings.Repeat("a", 40) + " refs/heads/main\n" + updatesEnd + "\n")
	if refs, quarantine, ok, err := ch4.readUpdates(); err != nil || !ok || quarantine != "/tmp/q" || len(refs) != 1 || refs[0].Ref != "refs/heads/main" {
		t.Fatalf("quarantine line: %v %q %v %v", refs, quarantine, ok, err)
	}
	// A closed channel refuses both operations, and the channel cannot
	// be created in a missing directory.
	ch3.close()
	if err := ch3.writeVerdict("ok"); err == nil {
		t.Fatal("closed verdict FIFO accepted")
	}
	if _, _, _, err := ch3.readUpdates(); err == nil {
		t.Fatal("closed updates FIFO accepted")
	}
	if _, err := newHookChannel(dir + "/missing"); err == nil {
		t.Fatal("missing directory accepted")
	}
	// The hook is installed once and rewritten when it differs.
	repoDir := t.TempDir()
	if err := installHook(repoDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repoDir+"/hooks/pre-receive", []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installHook(repoDir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(repoDir + "/hooks/pre-receive"); string(b) != preReceiveHook {
		t.Fatal("hook not rewritten")
	}
	if err := installHook(repoDir); err != nil {
		t.Fatal(err)
	}
	if err := installHook("/dev/null/x"); err == nil {
		t.Fatal("unwritable hook path accepted")
	}
}
