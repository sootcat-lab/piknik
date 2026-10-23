//go:build !windows

package main

func clipboardSeq() uint32 { return 0 }

func clipboardSensitive() bool { return false }
