//go:build windows

package main

import (
	"syscall"
	"time"
	"unsafe"
)

var (
	user32                         = syscall.NewLazyDLL("user32.dll")
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")
	procRegisterClipboardFormatW   = user32.NewProc("RegisterClipboardFormatW")
	procIsClipboardFormatAvailable = user32.NewProc("IsClipboardFormatAvailable")
	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procGlobalLock                 = kernel32.NewProc("GlobalLock")
	procGlobalUnlock               = kernel32.NewProc("GlobalUnlock")
	procRtlMoveMemory              = kernel32.NewProc("RtlMoveMemory")
)

// clipboardSeq returns the clipboard sequence number, which changes on every
// clipboard write, or 0 when it is not available.
func clipboardSeq() uint32 {
	r, _, _ := procGetClipboardSequenceNumber.Call()
	return uint32(r)
}

// clipboardFormat returns the ID of a registered clipboard format, or 0.
func clipboardFormat(name string) uintptr {
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0
	}
	format, _, _ := procRegisterClipboardFormatW.Call(uintptr(unsafe.Pointer(p)))
	return format
}

func clipboardHas(format uintptr) bool {
	if format == 0 {
		return false
	}
	r, _, _ := procIsClipboardFormatAvailable.Call(format)
	return r != 0
}

// clipboardSensitive reports whether the clipboard owner asked monitors and
// cloud sync to skip the content, as password managers do. These are the
// markers Microsoft documents for clipboard history and cloud clipboard.
func clipboardSensitive() bool {
	if clipboardHas(clipboardFormat("ExcludeClipboardContentFromMonitorProcessing")) ||
		clipboardHas(clipboardFormat("Clipboard Viewer Ignore")) {
		return true
	}
	for _, name := range []string{"CanIncludeInClipboardHistory", "CanUploadToCloudClipboard"} {
		if v, ok := clipboardDword(clipboardFormat(name)); ok && v == 0 {
			return true
		}
	}
	return false
}

// clipboardDword reads a format holding a DWORD. Another program may hold
// the clipboard open, so opening it is retried briefly.
func clipboardDword(format uintptr) (uint32, bool) {
	if !clipboardHas(format) {
		return 0, false
	}
	opened := false
	for range 5 {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			opened = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !opened {
		return 0, false
	}
	defer procCloseClipboard.Call()
	h, _, _ := procGetClipboardData.Call(format)
	if h == 0 {
		return 0, false
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return 0, false
	}
	defer procGlobalUnlock.Call(h)
	var v uint32
	procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&v)), p, unsafe.Sizeof(v))
	return v, true
}
