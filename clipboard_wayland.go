package main

import (
	"errors"
	"os/exec"
	"strings"
)

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
