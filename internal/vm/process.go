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

	// In the new HCS API, stdio handles are not returned directly by HcsCreateProcess.
	// They require additional steps (HcsGetProcessStdio or similar) that depend on GCS.
	// Fail fast so the caller can fall back to serial console.
	if info.StdInput == 0 && info.StdOutput == 0 {
		return -1, fmt.Errorf("GCS stdio handles not available (new HCS API); use serial console")
	}

	// Wrap Windows HANDLEs as os.File so we can use them with io.Copy.
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
func (v *VM) InteractiveShell(pipeName string) error {
	conn, err := openPipeWithRetry(pipeName, 30*time.Second)
	if err != nil {
		return fmt.Errorf("open console pipe %q: %w", pipeName, err)
	}
	defer conn.Close()

	log.Printf("[vmrunner] connected to serial console %q", pipeName)

	// Set terminal to raw mode so keystrokes are sent immediately.
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, os.Stdin)
		done <- err
	}()
	go io.Copy(os.Stdout, conn)

	return <-done
}

// RunCommand sends a command over the serial console pipe and prints the output
// until a shell prompt is detected.
func (v *VM) RunCommand(pipeName string, args []string) error {
	conn, err := openPipeWithRetry(pipeName, 30*time.Second)
	if err != nil {
		return fmt.Errorf("open console pipe %q: %w", pipeName, err)
	}
	defer conn.Close()

	// Wait for initial prompt.
	if err := waitForPrompt(conn); err != nil {
		return fmt.Errorf("wait for prompt: %w", err)
	}

	cmd := shellJoin(args) + "\n"
	if _, err := fmt.Fprint(conn, cmd); err != nil {
		return fmt.Errorf("write command: %w", err)
	}

	// Read until next prompt.
	return collectUntilPrompt(conn, os.Stdout)
}

// openPipeWithRetry tries to open the named pipe every 100 ms until timeout.
func openPipeWithRetry(name string, timeout time.Duration) (io.ReadWriteCloser, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f, err := openNamedPipe(name)
		if err == nil {
			return f, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out waiting for pipe %q after %s", name, timeout)
}

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procCreateFileW    = kernel32.NewProc("CreateFileW")
)

func openNamedPipe(name string) (io.ReadWriteCloser, error) {
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}

	const (
		GENERIC_READ  = 0x80000000
		GENERIC_WRITE = 0x40000000
		OPEN_EXISTING = 3
		FILE_ATTRIBUTE_NORMAL = 0x80
	)

	h, _, lastErr := procCreateFileW.Call(
		uintptr(unsafe.Pointer(namePtr)),
		GENERIC_READ|GENERIC_WRITE,
		0,
		0,
		OPEN_EXISTING,
		FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if syscall.Handle(h) == syscall.InvalidHandle {
		return nil, lastErr
	}
	return os.NewFile(h, name), nil
}

// waitForPrompt reads from r until it sees a '#' or '$' prompt character.
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
		// Wrap args containing spaces in double quotes.
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
