//go:build windows

package vmcompute

import "syscall"

// HcsSystem is an opaque handle to an HCS compute system (VM).
type HcsSystem syscall.Handle

// HcsOperation is an opaque handle to an async HCS operation.
type HcsOperation syscall.Handle

// HcsProcess is an opaque handle to a process running inside an HCS compute system.
type HcsProcess syscall.Handle

// HcsProcessInformation holds stdio handle information for a process.
type HcsProcessInformation struct {
	ProcessId uint32
	Reserved  uint32
	StdInput  syscall.Handle
	StdOutput syscall.Handle
	StdError  syscall.Handle
}
