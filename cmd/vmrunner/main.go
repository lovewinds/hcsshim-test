//go:build windows

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/microsoft/hcsshim/vmrunner/internal/config"
	"github.com/microsoft/hcsshim/vmrunner/internal/vm"
)

func main() {
	var (
		imageDir    = flag.String("image-dir", `C:\source\hcsshim\vm-image`, "VM image directory (Windows path)")
		memoryMB    = flag.Uint("memory", 2048, "Memory size in MB")
		cpuCount    = flag.Uint("cpu", 2, "Number of virtual CPUs")
		interactive = flag.Bool("i", false, "Interactive shell mode")
		kernelArgs  = flag.String("kernel-args", "", "Override kernel command line")
		vmID        = flag.String("id", "vmrunner-vm", "VM identifier")
		debug       = flag.Bool("debug", false, "Print HCS JSON config before creating VM")
		trace       = flag.Bool("trace", false, "Log each stdin→pipe write (shows bytes sent to VM)")
		kill        = flag.String("kill", "", "Terminate a running VM by ID and exit")
	)
	flag.Parse()
	vm.Trace = *trace

	// Handle -kill before starting a new VM.
	if *kill != "" {
		if err := vm.Kill(*kill); err != nil {
			log.Fatalf("failed to kill VM %q: %v", *kill, err)
		}
		log.Printf("[vmrunner] VM %q terminated", *kill)
		return
	}

	args := flag.Args()

	// Default to interactive when no command is given.
	if len(args) == 0 && !*interactive {
		*interactive = true
	}

	cfg := config.VMConfig{
		ImageDir:   *imageDir,
		MemoryMB:   uint32(*memoryMB),
		CPUCount:   uint32(*cpuCount),
		KernelArgs: *kernelArgs,
		VMID:       *vmID,
	}

	pipeName := fmt.Sprintf(`\\.\pipe\%s-console`, *vmID)
	cfg.PipeName = pipeName

	if *debug {
		j, err := config.BuildJSON(cfg)
		if err != nil {
			log.Fatalf("config build error: %v", err)
		}
		log.Printf("[vmrunner] HCS config JSON:\n%s", j)
	}

	machine, err := vm.Start(cfg)
	if err != nil {
		log.Fatalf("failed to start VM: %v", err)
	}
	log.Printf("[vmrunner] VM %q started", *vmID)

	// Handle Ctrl+C / SIGTERM: shut down the VM gracefully.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("[vmrunner] received signal %v, shutting down VM", sig)
		if err := machine.Shutdown(); err != nil {
			log.Printf("[vmrunner] shutdown error: %v", err)
		}
		os.Exit(0)
	}()

	// Execute command or open interactive shell.
	if *interactive {
		if err := machine.InteractiveShell(pipeName); err != nil {
			log.Printf("[vmrunner] interactive shell ended: %v", err)
		}
	} else {
		// First try GCS-based process execution.
		exitCode, err := machine.RunProcess(args)
		if err != nil {
			log.Printf("[vmrunner] GCS process failed (%v), falling back to serial console", err)
			if consoleErr := machine.RunCommand(pipeName, args); consoleErr != nil {
				log.Fatalf("serial console command failed: %v", consoleErr)
			}
		} else if exitCode != 0 {
			log.Printf("[vmrunner] process exited with code %d", exitCode)
		}
	}

	// Shutdown VM after command completes.
	if err := machine.Shutdown(); err != nil {
		log.Printf("[vmrunner] shutdown error: %v", err)
	}
}
