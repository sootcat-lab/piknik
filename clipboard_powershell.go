package main

import (
	"encoding/base64"
	"errors"
	"os/exec"
	"strings"
)

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
