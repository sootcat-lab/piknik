package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log"
	"strings"
	"time"
	"unicode/utf8"
)

// Item kinds a clipboard backend can read and write.
type clipKind int

const (
	kindText clipKind = iota
	kindImage
)

// clipItem is one clipboard value: text, or an image with its media type.
// A sensitive item was marked by a password manager; its data is not read.
type clipItem struct {
	kind      clipKind
	mime      string
	data      []byte
	sensitive bool
}

// Images travel over the unmodified piknik protocol as a single line of ASCII
// so that clients which cannot handle binary never receive raw image bytes:
//
//	PiknikImage1 <mime> <base64>
//
// A receiver that does not understand the envelope (or has image sync off)
// ignores it instead of putting garbage on its clipboard.
const imageEnvelopePrefix = "PiknikImage1 "

// imageMimes is the order in which clipboard image flavours are preferred.
var imageMimes = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

// passwordManagerHint is the MIME type KeePassXC and others add to secrets
// they put on the clipboard, asking clipboard managers not to record them.
const passwordManagerHint = "x-kde-passwordManagerHint"

// RunWatch continuously mirrors the local clipboard to the Piknik staging
// server and applies remote changes locally.
//
// The server is polled with a backoff: every interval after a change, up to
// four times the interval while nothing changes. The local clipboard is only
// read when the backend reports a possible change.
func RunWatch(conf Conf, interval time.Duration, syncImages bool) error {
	if interval <= 0 {
		interval = 700 * time.Millisecond
	}
	clip, err := newClipboard(syncImages)
	if err != nil {
		return err
	}
	srv := &remote{conf: conf}
	w := &watcher{
		clip:   clip,
		images: syncImages,
		push:   srv.push,
		pull:   srv.pull,
	}
	w.init()

	maxPullDelay := 4 * interval
	pullDelay := interval
	nextPull := time.Now()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if w.syncLocal() {
			pullDelay = interval
		}
		if now := time.Now(); !now.Before(nextPull) {
			if w.syncRemote() {
				pullDelay = interval
			} else {
				pullDelay = min(2*pullDelay, maxPullDelay)
			}
			nextPull = now.Add(pullDelay)
		}
		<-ticker.C
	}
}

// watcher holds the sync state. Content is compared by the hash of its wire
// encoding, so local and remote values can be compared directly.
//
// A value is pushed when the local clipboard changes to something the server
// does not already hold, and applied when the server changes to something the
// local clipboard does not already hold. Comparing against the last value on
// each side, rather than a history of seen values, keeps echoes out while
// still syncing a value that was copied before.
type watcher struct {
	clip   clipboardAdapter
	images bool
	push   func([]byte) error
	pull   func() ([]byte, error)

	localHash  [sha256.Size]byte // last value seen on, or written to, the local clipboard
	remoteHash [sha256.Size]byte // last value seen on, or pushed to, the server
	pending    *clipItem         // local change not pushed yet

	lastPushErr, lastPullErr string // logged once until the error changes
}

// logOnce logs err unless it repeats the previous error stored in *last.
func logOnce(last *string, format string, err error) {
	if msg := err.Error(); msg != *last {
		*last = msg
		log.Printf(format, err)
	}
}

// sensitiveHash stands for "a password manager secret is on the clipboard".
var sensitiveHash = sha256.Sum256([]byte("piknik: sensitive clipboard content"))

// init records the current clipboard without pushing it, so that starting
// the watcher never overwrites newer content on the server.
func (w *watcher) init() {
	w.clip.Changed()
	if item, err := w.clip.Get(); err == nil && item != nil && !item.sensitive {
		w.localHash = sha256.Sum256(item.encode())
	}
}

// syncLocal pushes a local change, if any. It reports whether it pushed.
func (w *watcher) syncLocal() bool {
	if w.clip.Changed() {
		item, err := w.clip.Get()
		switch {
		case err != nil:
			log.Printf("watch: read clipboard failed: %v", err)
		case item == nil:
			w.localHash = [sha256.Size]byte{}
			w.pending = nil
		case item.sensitive:
			if w.localHash != sensitiveHash {
				log.Printf("watch: skipped content marked as sensitive by a password manager")
			}
			w.localHash = sensitiveHash
			w.pending = nil
		default:
			if h := sha256.Sum256(item.encode()); h != w.localHash {
				w.localHash = h
				w.pending = nil
				if h != w.remoteHash {
					w.pending = item
				}
			}
		}
	}
	if w.pending == nil {
		return false
	}
	data := w.pending.encode()
	if err := w.push(data); err != nil {
		logOnce(&w.lastPushErr, "watch: push failed: %v", err)
		return false
	}
	w.lastPushErr = ""
	w.remoteHash = sha256.Sum256(data)
	log.Printf("watch: pushed %d bytes (%s)", len(w.pending.data), w.pending.kind)
	w.pending = nil
	return true
}

// syncRemote applies a remote change, if any. It reports whether the server
// content changed.
func (w *watcher) syncRemote() bool {
	raw, err := w.pull()
	if err != nil {
		if errors.Is(err, errClipboardUnchanged) || errors.Is(err, errClipboardEmpty) || errors.Is(err, errClipboardTooOld) {
			w.lastPullErr = ""
		} else {
			logOnce(&w.lastPullErr, "watch: pull failed: %v", err)
		}
		return false
	}
	w.lastPullErr = ""
	h := sha256.Sum256(raw)
	if len(raw) == 0 || h == w.remoteHash {
		return false
	}
	w.remoteHash = h
	item := decodeItem(raw, w.images)
	switch {
	case item == nil:
		// Not text and not an image envelope: a peer mangled binary, or
		// sent something this build cannot use.
		log.Printf("watch: ignored %d bytes that are not text or a usable image", len(raw))
		return true
	case h == w.localHash, w.pending != nil:
		// Already on the clipboard, or a newer local value is waiting to
		// be pushed.
		return true
	}
	if err := w.clip.Set(item); err != nil {
		log.Printf("watch: set clipboard failed: %v", err)
		w.remoteHash = [sha256.Size]byte{} // retry on the next pull
		return true
	}
	log.Printf("watch: applied %d bytes (%s)", len(item.data), item.kind)
	// Read the value back: the platform may normalize it (Windows re-encodes
	// images, for example), and that must not count as a local change.
	w.clip.Changed()
	w.localHash = h
	if cur, err := w.clip.Get(); err == nil && cur != nil && !cur.sensitive {
		w.localHash = sha256.Sum256(cur.encode())
	}
	return true
}

// remote pushes and pulls clipboard content with the copy and paste
// operations. It remembers the signature of the content last pushed or
// pulled, so that a pull does not download that content again.
type remote struct {
	conf      Conf
	signature []byte
}

func (s *remote) push(data []byte) error {
	client, h1, err := dialClient(s.conf, 6)
	if err != nil {
		return err
	}
	defer client.conn.Close()
	if err := client.copyOperation(h1, bytes.NewReader(data)); err != nil {
		return err
	}
	s.signature = client.signature
	return nil
}

func (s *remote) pull() ([]byte, error) {
	client, h1, err := dialClient(s.conf, 6)
	if err != nil {
		return nil, err
	}
	defer client.conn.Close()
	client.knownSignature = s.signature
	var out bytes.Buffer
	if err := client.pasteOperation(h1, false, &out); err != nil {
		return nil, err
	}
	s.signature = client.signature
	return out.Bytes(), nil
}

// ---------------------------------------------------------------------------
// Content encoding
// ---------------------------------------------------------------------------

func (k clipKind) String() string {
	if k == kindImage {
		return "image"
	}
	return "text"
}

func (item *clipItem) encode() []byte {
	if item.kind != kindImage {
		return item.data
	}
	return []byte(imageEnvelopePrefix + item.mime + " " +
		base64.StdEncoding.EncodeToString(item.data))
}

// decodeItem parses clipboard content coming from the server. It returns nil
// for anything that must not be written to the local clipboard: binary junk
// from a peer, malformed envelopes, or images when image sync is off.
func decodeItem(raw []byte, syncImages bool) *clipItem {
	// Check the envelope first: it is valid UTF-8 text, so a text-first
	// check would put "PiknikImage1 image/png iVBOR..." on the clipboard.
	if !bytes.HasPrefix(raw, []byte(imageEnvelopePrefix)) {
		if text, ok := plainTextItem(raw); ok {
			return text
		}
		return nil
	}
	rest := string(raw[len(imageEnvelopePrefix):])
	mime, payload, ok := strings.Cut(rest, " ")
	if !ok || !syncImages {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || sniffImageMime(data) != mime {
		return nil
	}
	return &clipItem{kind: kindImage, mime: mime, data: data}
}

// plainTextItem accepts only content that survives a text clipboard round trip.
// Invalid UTF-8 and NUL bytes mean the value is binary (a peer truncated an
// image at its first NUL, for example), and must be dropped.
func plainTextItem(data []byte) (*clipItem, bool) {
	if len(data) == 0 || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, false
	}
	return &clipItem{kind: kindText, data: data}, true
}

// sniffImageMime identifies the formats every backend here can re-encode.
func sniffImageMime(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return "image/png"
	case bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return ""
}
