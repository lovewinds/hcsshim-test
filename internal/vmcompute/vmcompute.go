//go:build windows

// Package vmcompute binds the HCS (Host Compute Service) synchronous API exported
// by vmcompute.dll (old-style, pre-computecore.h).
//
// Several calls return HCS_OPERATION_PENDING (0xC0370103) to signal an async
// operation. In that case the system handle IS valid, and callers must wait for
// the corresponding HCS_NOTIFICATION_TYPE via the callback mechanism.
package vmcompute

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

var (
	modVmcompute = syscall.NewLazyDLL("vmcompute.dll")
	modOle32     = syscall.NewLazyDLL("ole32.dll")

	// Compute system lifecycle.
	// Old API signature pattern: func(…args…, *System, *Result) HRESULT
	procHcsCreateComputeSystem    = modVmcompute.NewProc("HcsCreateComputeSystem")
	procHcsOpenComputeSystem      = modVmcompute.NewProc("HcsOpenComputeSystem")
	procHcsStartComputeSystem     = modVmcompute.NewProc("HcsStartComputeSystem")
	procHcsShutdownComputeSystem  = modVmcompute.NewProc("HcsShutdownComputeSystem")
	procHcsTerminateComputeSystem = modVmcompute.NewProc("HcsTerminateComputeSystem")
	procHcsCloseComputeSystem     = modVmcompute.NewProc("HcsCloseComputeSystem")

	// Async completion: register/unregister a callback on a system handle.
	procHcsRegisterComputeSystemCallback   = modVmcompute.NewProc("HcsRegisterComputeSystemCallback")
	procHcsUnregisterComputeSystemCallback = modVmcompute.NewProc("HcsUnregisterComputeSystemCallback")

	// Process management.
	procHcsCreateProcess    = modVmcompute.NewProc("HcsCreateProcess")
	procHcsCloseProcess     = modVmcompute.NewProc("HcsCloseProcess")
	procHcsTerminateProcess = modVmcompute.NewProc("HcsTerminateProcess")
	procHcsGetProcessInfo   = modVmcompute.NewProc("HcsGetProcessInfo")

	procCoTaskMemFree = modOle32.NewProc("CoTaskMemFree")
)

// errOperationPending (HCS_OPERATION_PENDING = 0xC0370103) is returned when an
// HCS call is accepted but completes asynchronously. The system handle is valid.
const errOperationPending = uintptr(0xC0370103)

// hcsNotificationType mirrors the HCS_NOTIFICATION_TYPE enum.
type hcsNotificationType uint32

const (
	hcsNotificationSystemExited          hcsNotificationType = 0x00000001
	hcsNotificationSystemCreateCompleted hcsNotificationType = 0x00000002
	hcsNotificationSystemStartCompleted  hcsNotificationType = 0x00000003
)

// --- Memory helpers ---

func freeCoTaskMem(ptr *uint16) {
	if ptr != nil {
		procCoTaskMemFree.Call(uintptr(unsafe.Pointer(ptr)))
	}
}

func ptrToString(ptr *uint16) string {
	if ptr == nil {
		return ""
	}
	return syscall.UTF16ToString((*[4096]uint16)(unsafe.Pointer(ptr))[:])
}

// --- Error helpers ---

func hresultError(hr uintptr, detail string) error {
	if hr == 0 {
		return nil
	}
	var msgBuf [512]uint16
	n, _ := syscall.FormatMessage(
		syscall.FORMAT_MESSAGE_FROM_SYSTEM|syscall.FORMAT_MESSAGE_IGNORE_INSERTS,
		0, uint32(hr), 0, msgBuf[:], nil,
	)
	sysMsg := ""
	if n > 0 {
		sysMsg = syscall.UTF16ToString(msgBuf[:n])
	}
	switch {
	case detail != "" && sysMsg != "":
		return fmt.Errorf("HRESULT 0x%08X (%s): %s", uint32(hr), sysMsg, detail)
	case detail != "":
		return fmt.Errorf("HRESULT 0x%08X: %s", uint32(hr), detail)
	case sysMsg != "":
		return fmt.Errorf("HRESULT 0x%08X (%s)", uint32(hr), sysMsg)
	default:
		return fmt.Errorf("HRESULT 0x%08X", uint32(hr))
	}
}

// --- Async wait helper ---

// waitForSystemNotification registers a one-shot callback on system, waits for
// the specified notification type, then unregisters. Call this AFTER the HCS
// function returns HCS_OPERATION_PENDING with a valid (non-zero) system handle.
//
// Windows amd64 uses a single calling convention, so syscall.NewCallback works.
func waitForSystemNotification(system HcsSystem, want hcsNotificationType, timeout time.Duration) error {
	ch := make(chan error, 1)

	cb := syscall.NewCallback(func(notType, _ /*ctx*/, status, data uintptr) uintptr {
		if hcsNotificationType(notType) == want {
			var err error
			if int32(status) < 0 {
				dataStr := ""
				if data != 0 {
					dataStr = ptrToString((*uint16)(unsafe.Pointer(data)))
				}
				err = hresultError(status, dataStr)
			}
			select {
			case ch <- err:
			default:
			}
		}
		return 0
	})

	var callbackHandle uintptr
	hr, _, _ := procHcsRegisterComputeSystemCallback.Call(
		uintptr(system),
		cb,
		0, // context — not needed; closure captures ch
		uintptr(unsafe.Pointer(&callbackHandle)),
	)
	if hr != 0 {
		return hresultError(hr, "HcsRegisterComputeSystemCallback")
	}
	defer procHcsUnregisterComputeSystemCallback.Call(callbackHandle)

	select {
	case err := <-ch:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("timeout after %s waiting for HCS notification 0x%X", timeout, uint32(want))
	}
}

// --- Public HCS API ---

// HcsCreateComputeSystem creates a new compute system (VM).
//
// Old API: HcsCreateComputeSystem(Id, Configuration, Identity, *System, *Result)
// Identity is a SECURITY_DESCRIPTOR HANDLE; 0 = default security descriptor.
func HcsCreateComputeSystem(id, configuration string) (HcsSystem, error) {
	idPtr, err := syscall.UTF16PtrFromString(id)
	if err != nil {
		return 0, err
	}
	configPtr, err := syscall.UTF16PtrFromString(configuration)
	if err != nil {
		return 0, err
	}

	var system HcsSystem
	var result *uint16

	hr, _, _ := procHcsCreateComputeSystem.Call(
		uintptr(unsafe.Pointer(idPtr)),
		uintptr(unsafe.Pointer(configPtr)),
		0, // Identity HANDLE = NULL
		uintptr(unsafe.Pointer(&system)),
		uintptr(unsafe.Pointer(&result)),
	)
	detail := ptrToString(result)
	freeCoTaskMem(result)

	if hr != 0 && hr != errOperationPending {
		return 0, hresultError(hr, detail)
	}
	if hr == errOperationPending {
		if err := waitForSystemNotification(system, hcsNotificationSystemCreateCompleted, 60*time.Second); err != nil {
			procHcsCloseComputeSystem.Call(uintptr(system))
			return 0, fmt.Errorf("wait for create: %w", err)
		}
	}
	return system, nil
}

// HcsOpenComputeSystem opens a handle to an existing compute system by ID.
//
// Old API: HcsOpenComputeSystem(Id, *System, *Result)
func HcsOpenComputeSystem(id string) (HcsSystem, error) {
	idPtr, err := syscall.UTF16PtrFromString(id)
	if err != nil {
		return 0, err
	}

	var system HcsSystem
	var result *uint16

	hr, _, _ := procHcsOpenComputeSystem.Call(
		uintptr(unsafe.Pointer(idPtr)),
		uintptr(unsafe.Pointer(&system)),
		uintptr(unsafe.Pointer(&result)),
	)
	detail := ptrToString(result)
	freeCoTaskMem(result)

	return system, hresultError(hr, detail)
}

// HcsStartComputeSystem starts a previously created compute system.
//
// Old API: HcsStartComputeSystem(System, Options, *Result)
func HcsStartComputeSystem(system HcsSystem, options string) error {
	var optionsPtr *uint16
	if options != "" {
		var err error
		optionsPtr, err = syscall.UTF16PtrFromString(options)
		if err != nil {
			return err
		}
	}

	var result *uint16
	hr, _, _ := procHcsStartComputeSystem.Call(
		uintptr(system),
		uintptr(unsafe.Pointer(optionsPtr)),
		uintptr(unsafe.Pointer(&result)),
	)
	detail := ptrToString(result)
	freeCoTaskMem(result)

	if hr != 0 && hr != errOperationPending {
		return hresultError(hr, detail)
	}
	if hr == errOperationPending {
		return waitForSystemNotification(system, hcsNotificationSystemStartCompleted, 120*time.Second)
	}
	return nil
}

// HcsShutdownComputeSystem requests a graceful shutdown of the compute system.
//
// Old API: HcsShutdownComputeSystem(System, Options, *Result)
func HcsShutdownComputeSystem(system HcsSystem, options string) error {
	var optionsPtr *uint16
	if options != "" {
		var err error
		optionsPtr, err = syscall.UTF16PtrFromString(options)
		if err != nil {
			return err
		}
	}

	var result *uint16
	hr, _, _ := procHcsShutdownComputeSystem.Call(
		uintptr(system),
		uintptr(unsafe.Pointer(optionsPtr)),
		uintptr(unsafe.Pointer(&result)),
	)
	detail := ptrToString(result)
	freeCoTaskMem(result)

	if hr != 0 && hr != errOperationPending {
		return hresultError(hr, detail)
	}
	if hr == errOperationPending {
		return waitForSystemNotification(system, hcsNotificationSystemExited, 30*time.Second)
	}
	return nil
}

// HcsTerminateComputeSystem forcibly terminates the compute system.
//
// Old API: HcsTerminateComputeSystem(System, Options, *Result)
func HcsTerminateComputeSystem(system HcsSystem, options string) error {
	var optionsPtr *uint16
	if options != "" {
		var err error
		optionsPtr, err = syscall.UTF16PtrFromString(options)
		if err != nil {
			return err
		}
	}

	var result *uint16
	hr, _, _ := procHcsTerminateComputeSystem.Call(
		uintptr(system),
		uintptr(unsafe.Pointer(optionsPtr)),
		uintptr(unsafe.Pointer(&result)),
	)
	detail := ptrToString(result)
	freeCoTaskMem(result)

	if hr != 0 && hr != errOperationPending {
		return hresultError(hr, detail)
	}
	if hr == errOperationPending {
		return waitForSystemNotification(system, hcsNotificationSystemExited, 10*time.Second)
	}
	return nil
}

// HcsCloseComputeSystem closes the handle to the compute system.
//
// Old API: HcsCloseComputeSystem(System) → HRESULT
func HcsCloseComputeSystem(system HcsSystem) error {
	hr, _, _ := procHcsCloseComputeSystem.Call(uintptr(system))
	return hresultError(hr, "")
}

// HcsCreateProcess creates a new process inside the compute system via GCS.
//
// Old API: HcsCreateProcess(System, ProcessParams, *ProcessInfo, *Process, *Result)
func HcsCreateProcess(system HcsSystem, processParameters string) (HcsProcess, *HcsProcessInformation, error) {
	paramsPtr, err := syscall.UTF16PtrFromString(processParameters)
	if err != nil {
		return 0, nil, err
	}

	var process HcsProcess
	var procInfo HcsProcessInformation
	var result *uint16

	hr, _, _ := procHcsCreateProcess.Call(
		uintptr(system),
		uintptr(unsafe.Pointer(paramsPtr)),
		uintptr(unsafe.Pointer(&procInfo)),
		uintptr(unsafe.Pointer(&process)),
		uintptr(unsafe.Pointer(&result)),
	)
	detail := ptrToString(result)
	freeCoTaskMem(result)

	if hr != 0 && hr != errOperationPending {
		return 0, nil, hresultError(hr, detail)
	}
	return process, &procInfo, nil
}

// HcsCloseProcess closes the handle to a process.
func HcsCloseProcess(process HcsProcess) error {
	hr, _, _ := procHcsCloseProcess.Call(uintptr(process))
	return hresultError(hr, "")
}

// HcsTerminateProcess forcibly terminates a process.
//
// Old API: HcsTerminateProcess(Process, *Result)
func HcsTerminateProcess(process HcsProcess) error {
	var result *uint16
	hr, _, _ := procHcsTerminateProcess.Call(
		uintptr(process),
		uintptr(unsafe.Pointer(&result)),
	)
	detail := ptrToString(result)
	freeCoTaskMem(result)
	return hresultError(hr, detail)
}

// HcsGetProcessInfo retrieves information about a process.
//
// Old API: HcsGetProcessInfo(Process, *ProcessInfo, *Result)
func HcsGetProcessInfo(process HcsProcess) (*HcsProcessInformation, error) {
	var procInfo HcsProcessInformation
	var result *uint16

	hr, _, _ := procHcsGetProcessInfo.Call(
		uintptr(process),
		uintptr(unsafe.Pointer(&procInfo)),
		uintptr(unsafe.Pointer(&result)),
	)
	detail := ptrToString(result)
	freeCoTaskMem(result)

	if err := hresultError(hr, detail); err != nil {
		return nil, err
	}
	return &procInfo, nil
}
