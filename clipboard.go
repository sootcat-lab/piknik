package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

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
