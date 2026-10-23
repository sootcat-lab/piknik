//go:build !linux

package main

import "syscall"

func childProcAttr() *syscall.SysProcAttr { return nil }
