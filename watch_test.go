package main

import (
	"crypto/rand"
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ed25519"
)

// fakeClip is an in-memory clipboard that reports every write as a change.
type fakeClip struct {
	item    *clipItem
	changed bool
}

func (c *fakeClip) Get() (*clipItem, error) { return c.item, nil }
func (c *fakeClip) Set(item *clipItem) error {
	c.item, c.changed = item, true
	return nil
}
func (c *fakeClip) Changed() bool {
	changed := c.changed
	c.changed = false
	return changed
}

// peer is a watcher sharing a fake server with other peers.
type peer struct {
	w    *watcher
	clip *fakeClip
}

func newPeers(n int) ([]*peer, *[]byte) {
	server := new([]byte)
	var peers []*peer
	for range n {
		clip := &fakeClip{}
		w := &watcher{
			clip: clip,
			push: func(data []byte) error { *server = append([]byte(nil), data...); return nil },
			pull: func() ([]byte, error) {
				if *server == nil {
					return nil, errClipboardEmpty
				}
				return *server, nil
			},
		}
		w.init()
		peers = append(peers, &peer{w, clip})
	}
	return peers, server
}

func (p *peer) copy(s string)      { p.clip.Set(&clipItem{kind: kindText, data: []byte(s)}) }
func (p *peer) text() string       { return string(p.clip.item.data) }
func (p *peer) tick() (bool, bool) { return p.w.syncLocal(), p.w.syncRemote() }

func settle(peers []*peer) {
	for range 3 {
		for _, p := range peers {
			p.tick()
		}
	}
}

func TestWatchRecopiedValueSyncs(t *testing.T) {
	peers, server := newPeers(2)
	a, b := peers[0], peers[1]
	for _, s := range []string{"X", "Y", "X"} {
		a.copy(s)
		settle(peers)
		if string(*server) != s || b.text() != s {
			t.Fatalf("after A copied %q: server=%q B=%q", s, *server, b.text())
		}
	}
	// B copies a value A pushed earlier.
	b.copy("Y")
	settle(peers)
	if a.text() != "Y" {
		t.Fatalf("A=%q, want Y", a.text())
	}
}

func TestWatchNoEcho(t *testing.T) {
	peers, _ := newPeers(1)
	a := peers[0]
	a.copy("X")
	if pushed, _ := a.tick(); !pushed {
		t.Fatal("local change not pushed")
	}
	for range 3 {
		if pushed, _ := a.tick(); pushed || a.clip.changed {
			t.Fatal("own push echoed back")
		}
	}
}

func TestWatchStartupDoesNotPush(t *testing.T) {
	server := []byte("remote")
	clip := &fakeClip{item: &clipItem{kind: kindText, data: []byte("local")}, changed: true}
	w := &watcher{
		clip: clip,
		push: func([]byte) error { t.Fatal("pushed at startup"); return nil },
		pull: func() ([]byte, error) { return server, nil },
	}
	w.init()
	w.syncLocal()
	w.syncRemote()
	if string(clip.item.data) != "remote" {
		t.Fatalf("clipboard=%q, want remote", clip.item.data)
	}
}

func TestWatchSkipsSensitive(t *testing.T) {
	peers, server := newPeers(1)
	a := peers[0]
	a.clip.Set(&clipItem{sensitive: true})
	if pushed, _ := a.tick(); pushed || *server != nil {
		t.Fatal("sensitive content pushed")
	}
}

func TestWatchRetriesFailedPush(t *testing.T) {
	peers, server := newPeers(1)
	a := peers[0]
	push := a.w.push
	a.w.push = func([]byte) error { return errors.New("down") }
	a.copy("X")
	a.tick()
	a.w.push = push
	a.tick()
	if string(*server) != "X" {
		t.Fatalf("server=%q, want X", *server)
	}
}

func TestDecodeItemRejectsBinary(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte("\x89PNG\r\n\x1a\n"),
		[]byte("PiknikImage1 image/png AAAA"),
		{0xff, 0xfe},
	} {
		if item := decodeItem(raw, true); item != nil {
			t.Errorf("decodeItem(%q) = %v, want nil", raw, item)
		}
	}
}

// startTestServer runs a real server on a free local port.
func startTestServer(t *testing.T) Conf {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	signPk, signSk, _ := ed25519.GenerateKey(rand.Reader)
	conf := Conf{
		Connect:        addr,
		Listen:         addr,
		MaxClients:     10,
		TrustedIPCount: 1,
		MaxLen:         1 << 20,
		EncryptSk:      make([]byte, 32),
		EncryptSkID:    make([]byte, 8),
		Psk:            make([]byte, 32),
		SignPk:         signPk,
		SignSk:         signSk,
		Timeout:        5 * time.Second,
		DataTimeout:    5 * time.Second,
		TTL:            time.Hour,
	}
	rand.Read(conf.EncryptSk)
	rand.Read(conf.Psk)
	go RunServer(conf)
	for range 50 {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return conf
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server did not start")
	return conf
}

func TestRemoteSkipsKnownContent(t *testing.T) {
	conf := startTestServer(t)
	a, b := &remote{conf: conf}, &remote{conf: conf}

	if err := a.push([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	// The pusher already has the content.
	if _, err := a.pull(); !errors.Is(err, errClipboardUnchanged) {
		t.Fatalf("pusher pull: err=%v, want unchanged", err)
	}
	// A peer downloads it once.
	if data, err := b.pull(); err != nil || string(data) != "hello" {
		t.Fatalf("peer pull: %q, %v", data, err)
	}
	if _, err := b.pull(); !errors.Is(err, errClipboardUnchanged) {
		t.Fatalf("peer second pull: err=%v, want unchanged", err)
	}
	// New content is downloaded again.
	if err := b.push([]byte("world")); err != nil {
		t.Fatal(err)
	}
	if data, err := a.pull(); err != nil || string(data) != "world" {
		t.Fatalf("pull after change: %q, %v", data, err)
	}
}
