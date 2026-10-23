//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	user32                         = syscall.NewLazyDLL("user32.dll")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")
	procRegisterClipboardFormatW   = user32.NewProc("RegisterClipboardFormatW")
	procIsClipboardFormatAvailable = user32.NewProc("IsClipboardFormatAvailable")
)

// clipboardSeq returns the clipboard sequence number, which changes on every
// clipboard write, or 0 when it is not available.
func clipboardSeq() uint32 {
	r, _, _ := procGetClipboardSequenceNumber.Call()
	return uint32(r)
}

// clipboardSensitive reports whether the clipboard owner asked monitors to
// skip the content, as password managers do.
func clipboardSensitive() bool {
	name, err := syscall.UTF16PtrFromString("ExcludeClipboardContentFromMonitorProcessing")
	if err != nil {
		return false
	}
	format, _, _ := procRegisterClipboardFormatW.Call(uintptr(unsafe.Pointer(name)))
	if format == 0 {
		return false
	}
	r, _, _ := procIsClipboardFormatAvailable.Call(format)
	return r != 0
}
