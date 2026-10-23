package main

import (
	"errors"
	"os/exec"
)

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
