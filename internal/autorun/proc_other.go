//go:build !windows && !linux

package autorun

import "syscall"

func setDeathSignal(*syscall.SysProcAttr) {}
