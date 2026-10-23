//go:build linux

package main

import "syscall"

// childProcAttr makes a long-running child exit along with piknik.
func childProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
