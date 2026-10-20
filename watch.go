package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
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
type clipItem struct {
	kind clipKind
	mime string
	data []byte
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

// RunWatch continuously mirrors the local clipboard to the Piknik staging
// server and applies remote changes locally.
//
// It relies on runClientIO(), which is the error-returning variant of
// RunClient() added by the patch in PATCH.md.
//
// Two rules keep the sync from looping or corrupting the local clipboard:
//   - content this client pushed is never applied back to it;
//   - content applied from the server is never pushed back.
//
// Non-text content is ignored unless images are enabled, and remote data that
// is neither valid UTF-8 text nor a well-formed image envelope is dropped, so
// a peer that mangles binary cannot overwrite the local clipboard.
func RunWatch(conf Conf, interval time.Duration, syncImages bool) error {
	if interval <= 0 {
		interval = 700 * time.Millisecond
	}
	clip, err := newClipboard(syncImages)
	if err != nil {
		return err
	}

	// Content recently pushed or applied. A plain single "last value" cannot
	// track this: peers bounce several values back within a few ticks.
	synced := newSyncHistory(2*time.Minute, 64)
	ignored := newSyncHistory(10*time.Minute, 16)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		local, err := clip.Get()
		if err != nil {
			log.Printf("watch: read clipboard failed: %v", err)
		} else if local != nil && !synced.has(local) {
			if err := pushClip(conf, local.encode()); err != nil {
				log.Printf("watch: push failed: %v", err)
			} else {
				synced.add(local)
				log.Printf("watch: pushed %d bytes (%s)", len(local.data), local.kind)
			}
		}

		remote, err := pullClip(conf)
		switch {
		case err != nil:
			if !isBenignPullError(err) {
				log.Printf("watch: pull failed: %v", err)
			}
		case len(remote) > 0:
			item := decodeItem(remote, syncImages)
			switch {
			case item == nil:
				// Not text and not an image envelope: a peer mangled
				// binary, or sent something this build cannot use.
				if !ignored.has(&clipItem{data: remote}) {
					ignored.add(&clipItem{data: remote})
					log.Printf("watch: ignored %d bytes that are not text or a usable image", len(remote))
				}
			case synced.has(item):
				// Our own push coming back, or content already applied.
			default:
				if cur, err := clip.Get(); err == nil && cur != nil && cur.equal(item) {
					synced.add(item)
					break
				}
				if err := clip.Set(item); err != nil {
					log.Printf("watch: set clipboard failed: %v", err)
				} else {
					synced.add(item)
					log.Printf("watch: applied %d bytes (%s)", len(item.data), item.kind)
				}
			}
		}

		<-ticker.C
	}
}

// pushClip uploads data using the existing copy operation.
func pushClip(conf Conf, data []byte) error {
	return runClientIO(conf, true, false, false, false, "", bytes.NewReader(data), nil)
}

// pullClip downloads the current clipboard using the existing paste operation.
func pullClip(conf Conf) ([]byte, error) {
	var out bytes.Buffer
	err := runClientIO(conf, false, false, false, false, "", nil, &out)
	return out.Bytes(), err
}

// isBenignPullError reports errors that simply mean "nothing to paste".
func isBenignPullError(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "empty") || strings.Contains(s, "too old")
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

func (item *clipItem) equal(other *clipItem) bool {
	return item.kind == other.kind && item.mime == other.mime && bytes.Equal(item.data, other.data)
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

// syncHistory is a small TTL+size bounded set of content hashes.
type syncHistory struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[[sha256.Size]byte]time.Time
}

func newSyncHistory(ttl time.Duration, max int) *syncHistory {
	return &syncHistory{ttl: ttl, max: max, entries: make(map[[sha256.Size]byte]time.Time)}
}

func (h *syncHistory) has(item *clipItem) bool {
	key := item.hash()
	h.mu.Lock()
	defer h.mu.Unlock()
	seenAt, ok := h.entries[key]
	if !ok {
		return false
	}
	if time.Since(seenAt) > h.ttl {
		delete(h.entries, key)
		return false
	}
	return true
}

func (h *syncHistory) add(item *clipItem) {
	key := item.hash()
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries[key] = now
	if len(h.entries) <= h.max {
		return
	}
	for k, seenAt := range h.entries {
		if now.Sub(seenAt) > h.ttl || len(h.entries) > h.max {
			delete(h.entries, k)
		}
	}
}

func (item *clipItem) hash() [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte{byte(item.kind)})
	h.Write([]byte(item.mime))
	h.Write([]byte{0})
	h.Write(item.data)
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// ---------------------------------------------------------------------------
// Clipboard backends
// ---------------------------------------------------------------------------

type clipboardAdapter interface {
	// Get returns the clipboard content, or nil when there is nothing usable.
	Get() (*clipItem, error)
	Set(item *clipItem) error
}

func newClipboard(syncImages bool) (clipboardAdapter, error) {
	switch runtime.GOOS {
	case "windows":
		return &powershellClipboard{images: syncImages}, nil
	case "darwin":
		return &pbpasteClipboard{}, nil
	default:
		if isWayland() {
			if _, err := exec.LookPath("wl-paste"); err != nil {
				return nil, fmt.Errorf("wl-paste not found: install wl-clipboard")
			}
			return &waylandClipboard{images: syncImages}, nil
		}
		if _, err := exec.LookPath("xclip"); err != nil {
			return nil, fmt.Errorf("no clipboard backend found: install wl-clipboard or xclip")
		}
		return &xclipClipboard{images: syncImages}, nil
	}
}

func isWayland() bool {
	return os.Getenv("WAYLAND_DISPLAY") != "" ||
		strings.Contains(strings.ToLower(os.Getenv("XDG_SESSION_TYPE")), "wayland")
}

// pickTextType prefers the richest text flavour the owner offers.
func pickTextType(types []string) string {
	for _, want := range []string{"text/plain;charset=utf-8", "text/plain", "UTF8_STRING", "TEXT", "STRING"} {
		for _, t := range types {
			if strings.EqualFold(strings.TrimSpace(t), want) {
				return t
			}
		}
	}
	return ""
}

func hasType(types []string, want string) bool {
	for _, t := range types {
		if strings.EqualFold(strings.TrimSpace(t), want) {
			return true
		}
	}
	return false
}

// runCapture runs a clipboard read command and returns its stdout.
func runCapture(command string, args ...string) ([]byte, error) {
	return exec.Command(command, args...).Output()
}

// runFeed pipes data into a clipboard write command.
//
// wl-copy and xclip fork a daemon that keeps serving the selection and
// inherits the command's stdout. Waiting on a captured stdout pipe would then
// block until that daemon exits, so stdout goes to /dev/null and only the exit
// status of the command we started is awaited.
func runFeed(command string, data []byte, args ...string) error {
	cmd := exec.Command(command, args...)
	cmd.Stdin = bytes.NewReader(data)
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()
	cmd.Stdout = devNull
	return cmd.Run()
}

// waylandClipboard uses wl-paste/wl-copy.
type waylandClipboard struct {
	images bool
}

func (c *waylandClipboard) types() []string {
	out, err := exec.Command("wl-paste", "--list-types").Output()
	if err != nil {
		return nil
	}
	var types []string
	for _, line := range strings.Split(string(out), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			types = append(types, t)
		}
	}
	return types
}

func (c *waylandClipboard) read(mime string) []byte {
	out, err := runCapture("wl-paste", "--no-newline", "--type", mime)
	if err != nil || len(out) == 0 {
		return nil
	}
	return out
}

func (c *waylandClipboard) Get() (*clipItem, error) {
	types := c.types()
	if len(types) == 0 {
		return nil, nil
	}
	if t := pickTextType(types); t != "" {
		if item, ok := plainTextItem(c.read(t)); ok {
			return item, nil
		}
	}
	if !c.images {
		return nil, nil
	}
	for _, mime := range imageMimes {
		if !hasType(types, mime) {
			continue
		}
		data := c.read(mime)
		if sniffImageMime(data) == mime {
			return &clipItem{kind: kindImage, mime: mime, data: data}, nil
		}
	}
	return nil, nil
}

func (c *waylandClipboard) Set(item *clipItem) error {
	if item.kind == kindImage {
		if !c.images {
			return errors.New("image sync is disabled")
		}
		return runFeed("wl-copy", item.data, "--type", item.mime)
	}
	return runFeed("wl-copy", item.data)
}

// pbpasteClipboard is the macOS backend: pbcopy/pbpaste handle text only.
type pbpasteClipboard struct{}

func (c *pbpasteClipboard) Get() (*clipItem, error) {
	out, err := exec.Command("pbpaste").Output()
	if err != nil {
		return nil, nil
	}
	item, ok := plainTextItem(out)
	if !ok {
		return nil, nil
	}
	return item, nil
}

func (c *pbpasteClipboard) Set(item *clipItem) error {
	if item.kind == kindImage {
		return errors.New("image sync is not implemented for pbcopy")
	}
	return runFeed("pbcopy", item.data)
}

// xclipClipboard is the X11 backend.
type xclipClipboard struct {
	images bool
}

func (c *xclipClipboard) targets() []string {
	out, err := exec.Command("xclip", "-selection", "clipboard", "-t", "TARGETS", "-o").Output()
	if err != nil {
		return nil
	}
	var targets []string
	for _, line := range strings.Split(string(out), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			targets = append(targets, t)
		}
	}
	return targets
}

func (c *xclipClipboard) read(target string) []byte {
	out, err := exec.Command("xclip", "-selection", "clipboard", "-t", target, "-o").Output()
	if err != nil || len(out) == 0 {
		return nil
	}
	return out
}

func (c *xclipClipboard) Get() (*clipItem, error) {
	targets := c.targets()
	if len(targets) == 0 {
		return nil, nil
	}
	if t := pickTextType(targets); t != "" {
		if item, ok := plainTextItem(c.read(t)); ok {
			return item, nil
		}
	}
	if !c.images {
		return nil, nil
	}
	for _, mime := range imageMimes {
		if !hasType(targets, mime) {
			continue
		}
		data := c.read(mime)
		if sniffImageMime(data) == mime {
			return &clipItem{kind: kindImage, mime: mime, data: data}, nil
		}
	}
	return nil, nil
}

func (c *xclipClipboard) Set(item *clipItem) error {
	if item.kind == kindImage {
		if !c.images {
			return errors.New("image sync is disabled")
		}
		return runFeed("xclip", item.data, "-selection", "clipboard", "-t", item.mime)
	}
	return runFeed("xclip", item.data, "-selection", "clipboard")
}

// powershellClipboard is a dependency-free Windows backend. It bypasses the
// console code page by reading and writing raw UTF-8 bytes on the standard
// streams, so non-ASCII clipboard text (e.g., Chinese) is not mangled. With
// images enabled it goes through System.Windows.Forms.Clipboard and exchanges
// PNG as base64.
type powershellClipboard struct {
	images bool
}

func (c *powershellClipboard) Get() (*clipItem, error) {
	const textScript = `$ErrorActionPreference = 'Stop'
$t = Get-Clipboard -Raw
if ($null -eq $t) { $t = '' }
$b = [System.Text.Encoding]::UTF8.GetBytes($t)
$o = [Console]::OpenStandardOutput()
$o.Write($b, 0, $b.Length)
$o.Flush()`
	out, err := exec.Command(
		"powershell", "-NoProfile", "-NonInteractive", "-STA",
		"-Command", textScript,
	).Output()
	if err != nil {
		return nil, err
	}
	if item, ok := plainTextItem(out); ok {
		return item, nil
	}
	if !c.images {
		return nil, nil
	}
	const imageScript = `$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$img = [System.Windows.Forms.Clipboard]::GetImage()
if ($null -eq $img) { exit 0 }
$ms = New-Object System.IO.MemoryStream
$img.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
[Console]::Out.Write([Convert]::ToBase64String($ms.ToArray()))`
	b64, err := exec.Command(
		"powershell", "-NoProfile", "-NonInteractive", "-STA",
		"-Command", imageScript,
	).Output()
	if err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b64)))
	if err != nil || sniffImageMime(data) != "image/png" {
		return nil, nil
	}
	return &clipItem{kind: kindImage, mime: "image/png", data: data}, nil
}

func (c *powershellClipboard) Set(item *clipItem) error {
	if item.kind == kindImage {
		if !c.images {
			return errors.New("image sync is disabled")
		}
		const imageScript = `$ErrorActionPreference = 'Stop'
$i = [Console]::OpenStandardInput()
$ms = New-Object System.IO.MemoryStream
$i.CopyTo($ms)
$b64 = [System.Text.Encoding]::ASCII.GetString($ms.ToArray()).Trim()
$bytes = [Convert]::FromBase64String($b64)
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$img = [System.Drawing.Image]::FromStream((New-Object System.IO.MemoryStream(,$bytes)))
[System.Windows.Forms.Clipboard]::SetImage($img)`
		return runFeed("powershell", []byte(base64.StdEncoding.EncodeToString(item.data)),
			"-NoProfile", "-NonInteractive", "-STA", "-Command", imageScript)
	}
	const textScript = `$ErrorActionPreference = 'Stop'
$i = [Console]::OpenStandardInput()
$ms = New-Object System.IO.MemoryStream
$i.CopyTo($ms)
$t = [System.Text.Encoding]::UTF8.GetString($ms.ToArray())
Set-Clipboard -Value $t`
	return runFeed("powershell", item.data,
		"-NoProfile", "-NonInteractive", "-Command", textScript)
}
