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
	"sync/atomic"
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
	w := &watcher{
		clip:   clip,
		images: syncImages,
		push:   func(data []byte) error { return pushClip(conf, data) },
		pull:   func() ([]byte, error) { return pullClip(conf) },
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
		if errors.Is(err, errClipboardEmpty) || errors.Is(err, errClipboardTooOld) {
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

// ---------------------------------------------------------------------------
// Clipboard backends
// ---------------------------------------------------------------------------

type clipboardAdapter interface {
	// Get returns the clipboard content, or nil when there is nothing usable.
	Get() (*clipItem, error)
	Set(item *clipItem) error
	// Changed reports whether the clipboard may have changed since the
	// last call. Backends that cannot tell always return true.
	Changed() bool
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
			c := &waylandClipboard{images: syncImages}
			c.startWatch()
			return c, nil
		}
		if _, err := exec.LookPath("xclip"); err != nil {
			return nil, fmt.Errorf("no clipboard backend found: install wl-clipboard or xclip")
		}
		c := &xclipClipboard{images: syncImages}
		if _, err := exec.LookPath("clipnotify"); err == nil {
			c.watchOneShot("clipnotify")
		} else {
			c.polling.Store(true)
		}
		return c, nil
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

// changeWatch tracks clipboard change notifications from a helper process.
// Until a helper is running, or once none can run, every tick reads the
// clipboard.
type changeWatch struct {
	dirty   atomic.Bool // a change notification arrived
	polling atomic.Bool // change notifications are unavailable
}

func (w *changeWatch) Changed() bool {
	return w.polling.Load() || w.dirty.Swap(false)
}

// watchLines runs a command that prints a line on every clipboard change,
// such as "wl-paste --watch". It is restarted if it exits after having run
// for a while, and abandoned for polling if it exits right away.
func (w *changeWatch) watchLines(name string, args ...string) {
	w.dirty.Store(true)
	go func() {
		for {
			started := time.Now()
			cmd := exec.Command(name, args...)
			cmd.SysProcAttr = childProcAttr()
			out, err := cmd.StdoutPipe()
			if err == nil {
				err = cmd.Start()
			}
			if err == nil {
				buf := make([]byte, 512)
				for {
					if _, err := out.Read(buf); err != nil {
						break
					}
					w.dirty.Store(true)
				}
				err = cmd.Wait()
			}
			if time.Since(started) < 5*time.Second {
				log.Printf("watch: %s unavailable (%v), polling instead", name, err)
				w.polling.Store(true)
				return
			}
			log.Printf("watch: %s exited (%v), restarting", name, err)
			w.dirty.Store(true)
			time.Sleep(time.Second)
		}
	}()
}

// watchOneShot runs a command that exits on the next clipboard change, such
// as clipnotify, in a loop. A failure switches to polling.
func (w *changeWatch) watchOneShot(name string, args ...string) {
	w.dirty.Store(true)
	go func() {
		for {
			cmd := exec.Command(name, args...)
			cmd.SysProcAttr = childProcAttr()
			if err := cmd.Run(); err != nil {
				log.Printf("watch: %s failed (%v), polling instead", name, err)
				w.polling.Store(true)
				return
			}
			w.dirty.Store(true)
		}
	}()
}

// waylandClipboard uses wl-paste/wl-copy. Changes are watched with
// "wl-paste --watch", which needs a data-control protocol; compositors
// without one (GNOME, for one) fall back to polling.
type waylandClipboard struct {
	changeWatch
	images bool
}

// startWatch runs wl-paste --watch. Where piknik cannot ask the kernel to
// stop it on exit, the notifier kills wl-paste once nobody reads its output.
func (c *waylandClipboard) startWatch() {
	c.watchLines("wl-paste", "--watch", "sh", "-c", `trap '' PIPE; echo || kill $PPID`)
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
	if hasType(types, passwordManagerHint) {
		return &clipItem{sensitive: true}, nil
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

// pbpasteClipboard is the macOS backend: pbcopy/pbpaste handle text only,
// and cannot see the markers password managers set, so secrets are synced.
// The clipboard is read on every tick.
type pbpasteClipboard struct{}

func (c *pbpasteClipboard) Changed() bool { return true }

func (c *pbpasteClipboard) Get() (*clipItem, error) {
	out, err := exec.Command("pbpaste").Output()
	if err != nil {
		return nil, err
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

// xclipClipboard is the X11 backend. xclip cannot wait for changes: with
// clipnotify installed the clipboard is read on change, otherwise on every
// tick.
type xclipClipboard struct {
	changeWatch
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
	if hasType(targets, passwordManagerHint) {
		return &clipItem{sensitive: true}, nil
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
// PNG as base64. PowerShell is slow to start, so it only runs when the
// clipboard sequence number changes.
type powershellClipboard struct {
	images bool
	seq    uint32
}

func (c *powershellClipboard) Changed() bool {
	seq := clipboardSeq()
	if seq == 0 { // no access to the sequence number: always read
		return true
	}
	changed := seq != c.seq
	c.seq = seq
	return changed
}

func (c *powershellClipboard) Get() (*clipItem, error) {
	if clipboardSensitive() {
		return &clipItem{sensitive: true}, nil
	}
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
