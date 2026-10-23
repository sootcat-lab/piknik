package main

import (
	"errors"
	"testing"
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
