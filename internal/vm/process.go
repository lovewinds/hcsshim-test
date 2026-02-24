//go:build windows

package vm

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"syscall"
	"time"
	"unsafe"

	"github.com/microsoft/hcsshim/vmrunner/internal/vmcompute"
)

// processParams is the JSON payload for HcsCreateProcess.
type processParams struct {
	ApplicationName  string            `json:"ApplicationName"`
	CommandLine      string            `json:"CommandLine"`
	User             string            `json:"User,omitempty"`
	WorkingDirectory string            `json:"WorkingDirectory"`
	Environment      map[string]string `json:"Environment,omitempty"`
	CreateStdInPipe  bool              `json:"CreateStdInPipe"`
	CreateStdOutPipe bool              `json:"CreateStdOutPipe"`
	CreateStdErrPipe bool              `json:"CreateStdErrPipe"`
	EmulateConsole   bool              `json:"EmulateConsole"`
}

// RunProcess runs a command inside the VM via GCS (HcsCreateProcess).
// It streams stdout/stderr to os.Stdout/os.Stderr and returns the process exit code.
func (v *VM) RunProcess(args []string) (int, error) {
	if len(args) == 0 {
		args = []string{"/bin/sh"}
	}

	cmdLine := shellJoin(args)
	params := processParams{
		ApplicationName:  args[0],
		CommandLine:      cmdLine,
		WorkingDirectory: "/",
		CreateStdInPipe:  true,
		CreateStdOutPipe: true,
		CreateStdErrPipe: true,
		EmulateConsole:   false,
	}

	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return -1, fmt.Errorf("marshal process params: %w", err)
	}

	log.Printf("[vmrunner] creating process via GCS: %s", cmdLine)
	proc, info, err := vmcompute.HcsCreateProcess(v.system, string(paramsJSON))
	if err != nil {
		return -1, fmt.Errorf("HcsCreateProcess: %w", err)
	}
	defer vmcompute.HcsCloseProcess(proc)

	// GCS stdio handles are not returned by this HCS API version.
	// Fail fast so the caller can fall back to the serial console.
	if info.StdInput == 0 && info.StdOutput == 0 {
		return -1, fmt.Errorf("GCS stdio handles not available; use serial console")
	}

	stdin  := os.NewFile(uintptr(info.StdInput),  "stdin")
	stdout := os.NewFile(uintptr(info.StdOutput), "stdout")
	stderr := os.NewFile(uintptr(info.StdError),  "stderr")

	go func() {
		defer stdin.Close()
		io.Copy(stdin, os.Stdin)
	}()

	done := make(chan struct{})
	go func() {
		io.Copy(os.Stdout, stdout)
		close(done)
	}()
	go io.Copy(os.Stderr, stderr)

	<-done
	stdout.Close()
	stderr.Close()
	return 0, nil
}

// InteractiveShell opens the serial console named pipe and connects it to the
// terminal bidirectionally (stdin ↔ pipe, pipe → stdout).
//
// The pipe is opened with FILE_FLAG_OVERLAPPED so that concurrent ReadFile and
// WriteFile on the same handle do not serialize.
//
// Console mode is switched to raw VTI inside stdinToPipe via prepareRawConsole.
// ReadFile is used directly on the console HANDLE (not os.Stdin.Read / ReadConsole)
// to avoid the ConPTY/Windows Terminal bug where SetConsoleMode causes indefinite
// blocking when going through the ReadConsole path.
func (v *VM) InteractiveShell(pipeName string) error {
	h, err := openOverlappedPipeWithRetry(pipeName, 30*time.Second)
	if err != nil {
		return fmt.Errorf("open console pipe %q: %w", pipeName, err)
	}
	defer syscall.CloseHandle(h)

	log.Printf("[vmrunner] connected to serial console %q", pipeName)

	readerDone := make(chan error, 1)
	go func() { readerDone <- pipeToStdout(h) }()

	writerDone := make(chan error, 1)
	go func() { writerDone <- stdinToPipe(h) }()

	select {
	case err := <-readerDone:
		return err
	case err := <-writerDone:
		return err
	}
}

// RunCommand sends a command over the serial console pipe and prints the output
// until a shell prompt is detected.
func (v *VM) RunCommand(pipeName string, args []string) error {
	f, err := openSyncPipeWithRetry(pipeName, 30*time.Second)
	if err != nil {
		return fmt.Errorf("open console pipe %q: %w", pipeName, err)
	}
	defer f.Close()

	// Wait for initial prompt.
	if err := waitForPrompt(f); err != nil {
		return fmt.Errorf("wait for prompt: %w", err)
	}

	cmd := shellJoin(args) + "\n"
	if _, err := fmt.Fprint(f, cmd); err != nil {
		return fmt.Errorf("write command: %w", err)
	}

	// Read until next prompt.
	return collectUntilPrompt(f, os.Stdout)
}

// Trace enables verbose I/O trace logging. Set via -trace flag in main.
var Trace bool

// kernel32 procedures for named pipe I/O and overlapped I/O.
var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procCreateFileW         = kernel32.NewProc("CreateFileW")
	procCreateEventW        = kernel32.NewProc("CreateEventW")
	procGetOverlappedResult = kernel32.NewProc("GetOverlappedResult")
	procGetConsoleMode      = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode      = kernel32.NewProc("SetConsoleMode")
)

const (
	genericReadWrite    = 0xC0000000 // GENERIC_READ | GENERIC_WRITE
	openExisting        = 3
	fileAttributeNormal = 0x80
	fileFlagOverlapped  = 0x40000000

	enableProcessedInput   = 0x0001 // keep Ctrl+C → SIGINT
	enableLineInput        = 0x0002 // line-buffering (remove for char-at-a-time)
	enableEchoInput        = 0x0004 // local echo (remove to prevent double echo)
	enableVirtualTermInput = 0x0200 // VTI mode: ReadFile returns VT sequences
)

// openOverlappedPipe opens a named pipe with FILE_FLAG_OVERLAPPED so that
// concurrent ReadFile and WriteFile on the same handle do not serialize.
func openOverlappedPipe(name string) (syscall.Handle, error) {
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return syscall.InvalidHandle, err
	}
	h, _, lastErr := procCreateFileW.Call(
		uintptr(unsafe.Pointer(namePtr)),
		genericReadWrite,
		0,
		0,
		openExisting,
		fileAttributeNormal|fileFlagOverlapped,
		0,
	)
	if syscall.Handle(h) == syscall.InvalidHandle {
		return syscall.InvalidHandle, lastErr
	}
	return syscall.Handle(h), nil
}

// openOverlappedPipeWithRetry retries every 100 ms until timeout.
func openOverlappedPipeWithRetry(name string, timeout time.Duration) (syscall.Handle, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		h, err := openOverlappedPipe(name)
		if err == nil {
			return h, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return syscall.InvalidHandle, fmt.Errorf("timed out waiting for pipe %q after %s", name, timeout)
}

// openSyncPipe opens a named pipe in synchronous (non-overlapped) mode.
// Suitable for RunCommand where reads and writes are sequential.
func openSyncPipe(name string) (*os.File, error) {
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, _, lastErr := procCreateFileW.Call(
		uintptr(unsafe.Pointer(namePtr)),
		genericReadWrite,
		0,
		0,
		openExisting,
		fileAttributeNormal,
		0,
	)
	if syscall.Handle(h) == syscall.InvalidHandle {
		return nil, lastErr
	}
	return os.NewFile(h, name), nil
}

// openSyncPipeWithRetry retries every 100 ms until timeout.
func openSyncPipeWithRetry(name string, timeout time.Duration) (*os.File, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f, err := openSyncPipe(name)
		if err == nil {
			return f, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out waiting for pipe %q after %s", name, timeout)
}

// createEvent creates a Win32 auto-reset, initially non-signaled event object.
func createEvent() (syscall.Handle, error) {
	h, _, lastErr := procCreateEventW.Call(0, 0, 0, 0)
	if h == 0 {
		return syscall.InvalidHandle, lastErr
	}
	return syscall.Handle(h), nil
}

// isPipeClose reports whether err means the remote end closed or disconnected.
func isPipeClose(err error) bool {
	if err == io.EOF {
		return true
	}
	if e, ok := err.(syscall.Errno); ok {
		return e == 109 || e == 233 // ERROR_BROKEN_PIPE, ERROR_PIPE_NOT_CONNECTED
	}
	return false
}

// pipeToStdout reads from h using overlapped I/O and writes to stdout.
func pipeToStdout(h syscall.Handle) error {
	ev, err := createEvent()
	if err != nil {
		return fmt.Errorf("createEvent: %w", err)
	}
	defer syscall.CloseHandle(ev)

	buf := make([]byte, 4096)
	for {
		var ol syscall.Overlapped
		ol.HEvent = ev

		var n uint32
		err := syscall.ReadFile(h, buf, &n, &ol)
		if err == syscall.ERROR_IO_PENDING {
			syscall.WaitForSingleObject(ev, syscall.INFINITE)
			r, _, lastErr := procGetOverlappedResult.Call(uintptr(h), uintptr(unsafe.Pointer(&ol)), uintptr(unsafe.Pointer(&n)), 0)
			if r == 0 {
				err = lastErr
			} else {
				err = nil
			}
		}
		if err != nil {
			if isPipeClose(err) {
				return nil
			}
			return err
		}
		if n > 0 {
			os.Stdout.Write(buf[:n])
		}
	}
}

// prepareRawConsole puts the Windows console stdin handle into raw VTI mode
// (no local echo, no line buffering) and returns the raw HANDLE, a restore
// function, and whether stdin is actually a console.
//
// ENABLE_PROCESSED_INPUT is kept so Ctrl+C continues to generate SIGINT.
// ReadFile on the returned handle bypasses ReadConsole, avoiding the
// ConPTY / Windows Terminal bug where SetConsoleMode causes indefinite blocking.
func prepareRawConsole() (h syscall.Handle, restore func(), isConsole bool) {
	stdinH, err := syscall.GetStdHandle(syscall.STD_INPUT_HANDLE)
	if err != nil {
		return syscall.InvalidHandle, nil, false
	}
	var old uint32
	r, _, _ := procGetConsoleMode.Call(uintptr(stdinH), uintptr(unsafe.Pointer(&old)))
	if r == 0 {
		// stdin is not a console (redirected pipe, file, etc.)
		return syscall.InvalidHandle, nil, false
	}
	newMode := (old &^ (enableLineInput | enableEchoInput)) | enableVirtualTermInput
	procSetConsoleMode.Call(uintptr(stdinH), uintptr(newMode))
	return stdinH, func() { procSetConsoleMode.Call(uintptr(stdinH), uintptr(old)) }, true
}

// stdinToPipe reads from stdin and writes to h using overlapped I/O.
// CR (\r) and CRLF (\r\n) are converted to LF (\n) for the Linux tty.
//
// When stdin is a Windows console, the console is put into raw VTI mode
// (no local echo, no line buffering) and ReadFile is used directly on the
// console handle. This bypasses the ReadConsole path that causes indefinite
// blocking under ConPTY/Windows Terminal after any SetConsoleMode call.
func stdinToPipe(h syscall.Handle) error {
	ev, err := createEvent()
	if err != nil {
		return fmt.Errorf("createEvent: %w", err)
	}
	defer syscall.CloseHandle(ev)

	// Switch the console to raw VTI mode (removes local echo and line
	// buffering). Falls back gracefully when stdin is redirected.
	stdinH, restore, isConsole := prepareRawConsole()
	if restore != nil {
		defer restore()
	}

	inBuf  := make([]byte, 256)
	outBuf := make([]byte, 0, 256)
	prevCR := false

	for {
		var n uint32
		var readErr error
		if isConsole {
			// Use ReadFile directly on the console HANDLE so we bypass
			// ReadConsole — which hangs in ConPTY after SetConsoleMode.
			readErr = syscall.ReadFile(stdinH, inBuf, &n, nil)
		} else {
			nn, e := os.Stdin.Read(inBuf)
			n, readErr = uint32(nn), e
		}
		if int(n) > 0 {
			outBuf = outBuf[:0]
			for _, b := range inBuf[:n] {
				switch {
				case b == '\r':
					outBuf = append(outBuf, '\n')
					prevCR = true
				case b == '\n' && prevCR:
					// CRLF: drop the LF, CR was already converted above.
					prevCR = false
				default:
					prevCR = false
					outBuf = append(outBuf, b)
				}
			}
			if len(outBuf) > 0 {
				var ol syscall.Overlapped
				ol.HEvent = ev
				var written uint32
				werr := syscall.WriteFile(h, outBuf, &written, &ol)
				if werr == syscall.ERROR_IO_PENDING {
					syscall.WaitForSingleObject(ev, syscall.INFINITE)
					r, _, lastErr := procGetOverlappedResult.Call(uintptr(h), uintptr(unsafe.Pointer(&ol)), uintptr(unsafe.Pointer(&written)), 0)
					if r == 0 {
						werr = lastErr
					} else {
						werr = nil
					}
				}
				if Trace {
					log.Printf("[vmrunner] trace: stdin→pipe %q written=%d", outBuf, written)
				}
				if werr != nil {
					if isPipeClose(werr) {
						return nil
					}
					return werr
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

// waitForPrompt reads from r until it sees a shell prompt (# or $).
func waitForPrompt(r io.Reader) error {
	buf := make([]byte, 1)
	var last byte
	for {
		_, err := r.Read(buf)
		if err != nil {
			return err
		}
		os.Stdout.Write(buf)
		if (buf[0] == '#' || buf[0] == '$') && last == ' ' {
			return nil
		}
		last = buf[0]
	}
}

// collectUntilPrompt copies r to w until a shell prompt is detected.
func collectUntilPrompt(r io.Reader, w io.Writer) error {
	buf := make([]byte, 1)
	var prev byte
	for {
		_, err := r.Read(buf)
		if err != nil {
			return err
		}
		w.Write(buf)
		if (buf[0] == '#' || buf[0] == '$') && prev == ' ' {
			return nil
		}
		prev = buf[0]
	}
}

// shellJoin joins args into a single command line string.
func shellJoin(args []string) string {
	if len(args) == 0 {
		return ""
	}
	result := ""
	for i, a := range args {
		if i > 0 {
			result += " "
		}
		if containsSpace(a) {
			result += `"` + a + `"`
		} else {
			result += a
		}
	}
	return result
}

func containsSpace(s string) bool {
	for _, c := range s {
		if c == ' ' {
			return true
		}
	}
	return false
}
