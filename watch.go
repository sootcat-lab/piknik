package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// RunWatch continuously mirrors the local clipboard to the Piknik staging
// server and applies remote changes locally.
//
// It relies on runClientIO(), which is the error-returning variant of
// RunClient() added by the patch in PATCH.md.
func RunWatch(conf Conf, interval time.Duration) error {
	if interval <= 0 {
		interval = 700 * time.Millisecond
	}
	clip, err := newClipboard()
	if err != nil {
		return err
	}

	var lastSynced, lastPushed []byte
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if local, err := clip.Get(); err == nil {
			if len(local) > 0 &&
				!sameContent(local, lastSynced) &&
				!sameContent(local, lastPushed) {
				if err := pushClip(conf, local); err != nil {
					log.Printf("watch: push failed: %v", err)
				} else {
					lastPushed = local
					lastSynced = local
					log.Printf("watch: pushed %d bytes", len(local))
				}
			}
		}

		remote, err := pullClip(conf)
		switch {
		case err != nil:
			if !isBenignPullError(err) {
				log.Printf("watch: pull failed: %v", err)
			}
		case len(remote) > 0 &&
			!sameContent(remote, lastSynced) &&
			!sameContent(remote, lastPushed):
			// Do not write if the clipboard already holds this content.
			if cur, _ := clip.Get(); sameContent(cur, remote) {
				lastSynced = remote
				lastPushed = remote
				break
			}
			if err := clip.Set(remote); err != nil {
				log.Printf("watch: set clipboard failed: %v", err)
			} else {
				lastSynced = remote
				lastPushed = remote
				log.Printf("watch: applied %d bytes", len(remote))
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

// sameContent compares clipboard values ignoring trailing line endings, which
// are added or stripped inconsistently by different clipboard backends.
func sameContent(a, b []byte) bool {
	return bytes.Equal(bytes.TrimRight(a, "\r\n"), bytes.TrimRight(b, "\r\n"))
}

// isBenignPullError reports errors that simply mean "nothing to paste".
func isBenignPullError(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "empty") || strings.Contains(s, "too old")
}

// ---------------------------------------------------------------------------
// Clipboard backends
// ---------------------------------------------------------------------------

type clipboardAdapter interface {
	Get() ([]byte, error)
	Set([]byte) error
}

func newClipboard() (clipboardAdapter, error) {
	switch runtime.GOOS {
	case "windows":
		return &powershellClipboard{}, nil
	case "darwin":
		return &execClipboard{
			getCmd: []string{"pbpaste"},
			setCmd: []string{"pbcopy"},
		}, nil
	default:
		if isWayland() {
			if _, err := exec.LookPath("wl-paste"); err != nil {
				return nil, fmt.Errorf("wl-paste not found: install wl-clipboard")
			}
			return &execClipboard{
				getCmd: []string{"wl-paste", "--no-newline"},
				setCmd: []string{"wl-copy"},
			}, nil
		}
		if _, err := exec.LookPath("xclip"); err == nil {
			return &execClipboard{
				getCmd: []string{"xclip", "-selection", "clipboard", "-o"},
				setCmd: []string{"xclip", "-selection", "clipboard"},
			}, nil
		}
		return nil, fmt.Errorf("no clipboard backend found: install wl-clipboard or xclip")
	}
}

func isWayland() bool {
	return os.Getenv("WAYLAND_DISPLAY") != "" ||
		strings.Contains(strings.ToLower(os.Getenv("XDG_SESSION_TYPE")), "wayland")
}

type execClipboard struct {
	getCmd []string
	setCmd []string
}

func (c *execClipboard) Get() ([]byte, error) {
	out, err := exec.Command(c.getCmd[0], c.getCmd[1:]...).Output()
	if err != nil {
		// Empty clipboard is reported as a non-zero exit by wl-paste/xclip.
		if _, ok := err.(*exec.ExitError); ok {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

func (c *execClipboard) Set(data []byte) error {
	cmd := exec.Command(c.setCmd[0], c.setCmd[1:]...)
	cmd.Stdin = bytes.NewReader(data)
	return cmd.Run()
}

// powershellClipboard is a dependency-free Windows backend. It spawns a
// PowerShell process on every poll, which is acceptable at 1s intervals but
// can be replaced by a native clipboard library later.
type powershellClipboard struct{}

func (c *powershellClipboard) Get() ([]byte, error) {
	out, err := exec.Command(
		"powershell", "-NoProfile", "-NonInteractive",
		"-Command", "Get-Clipboard -Raw",
	).Output()
	if err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out, []byte("\r\n")), nil
}

func (c *powershellClipboard) Set(data []byte) error {
	cmd := exec.Command(
		"powershell", "-NoProfile", "-NonInteractive",
		"-Command", "Set-Clipboard -Value ([Console]::In.ReadToEnd())",
	)
	cmd.Stdin = bytes.NewReader(data)
	return cmd.Run()
}
