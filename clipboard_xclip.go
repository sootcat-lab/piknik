package main

import (
	"errors"
	"os/exec"
	"strings"
)

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
