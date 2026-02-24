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
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "run":
			cmdRun(os.Args[2:])
			return
		case "exec":
			cmdExec(os.Args[2:])
			return
		case "list":
			cmdList()
			return
		case "attach":
			cmdAttach(os.Args[2:])
			return
		case "stop":
			cmdStop(os.Args[2:])
			return
		case "kill":
			cmdKill(os.Args[2:])
			return
		case "help", "-h", "--help", "-help":
			printUsage()
			return
		}
	}
	// Backward compatibility: no subcommand → treat all args as "run".
	cmdRun(os.Args[1:])
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: vmrunner <command> [flags] [args]

Commands:
  run    [flags]            Start a VM (detached, or interactive with -i)
  exec   [flags] <cmd...>  Run a command in a VM (starts VM if not running)
  list                     List running VMs
  attach <vm-id>           Connect to a running VM's serial console
  stop   <vm-id>           Gracefully shut down a running VM
  kill   <vm-id>           Forcibly terminate a running VM
  help                     Show this help

Run flags:
  -i                 Connect interactive shell (VM is shut down on exit)
  -id string         VM identifier (default "vmrunner-vm")
  -memory uint       Memory in MB (default 2048)
  -cpu uint          Number of virtual CPUs (default 2)
  -image-dir string  VM image directory (default C:\source\hcsshim\vm-image)
  -kernel-args       Override kernel command line
  -debug             Print HCS JSON config before creating VM

Exec flags:
  -id string         VM identifier to target (default "vmrunner-vm")
  -memory uint       Memory in MB if VM needs to be started (default 2048)
  -cpu uint          CPUs if VM needs to be started (default 2)
  -image-dir string  Image directory if VM needs to be started
  -debug             Print HCS JSON config if VM needs to be started

Examples:
  vmrunner run                        # start VM, detach
  vmrunner run -i                     # start VM, interactive shell
  vmrunner run -memory 4096 -cpu 4 -i
  vmrunner exec ls -la                # run command (start VM if needed)
  vmrunner exec -id my-vm ls -la
  vmrunner list
  vmrunner attach vmrunner-vm
  vmrunner stop   vmrunner-vm
  vmrunner kill   vmrunner-vm
`)
}

// runFlags holds flags shared between cmdRun and cmdExec.
type runFlags struct {
	imageDir   string
	memoryMB   uint
	cpuCount   uint
	kernelArgs string
	vmID       string
	debug      bool
}

func addRunFlags(fs *flag.FlagSet) *runFlags {
	f := &runFlags{}
	fs.StringVar(&f.imageDir,   "image-dir",    `C:\source\hcsshim\vm-image`, "VM image directory (Windows path)")
	fs.UintVar(&f.memoryMB,     "memory",        2048,          "Memory size in MB")
	fs.UintVar(&f.cpuCount,     "cpu",           2,             "Number of virtual CPUs")
	fs.StringVar(&f.kernelArgs, "kernel-args",  "",             "Override kernel command line")
	fs.StringVar(&f.vmID,       "id",            "vmrunner-vm", "VM identifier")
	fs.BoolVar(&f.debug,        "debug",         false,         "Print HCS JSON config before creating VM")
	return f
}

func (f *runFlags) vmConfig() config.VMConfig {
	cfg := config.VMConfig{
		ImageDir:   f.imageDir,
		MemoryMB:   uint32(f.memoryMB),
		CPUCount:   uint32(f.cpuCount),
		KernelArgs: f.kernelArgs,
		VMID:       f.vmID,
	}
	cfg.PipeName = fmt.Sprintf(`\\.\pipe\%s-console`, f.vmID)
	return cfg
}

// cmdRun starts a VM. With -i it attaches an interactive shell and shuts the
// VM down on exit. Without -i it detaches immediately (VM keeps running).
func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	f := addRunFlags(fs)
	interactive := fs.Bool("i", false, "Interactive shell mode (VM is shut down on exit)")
	trace := fs.Bool("trace", false, "") // superset of -debug; omitted from help
	_ = fs.Parse(args)

	if *trace {
		vm.Trace = true
		f.debug = true
	}

	cfg := f.vmConfig()

	if f.debug {
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
	log.Printf("[vmrunner] VM %q started", f.vmID)

	if !*interactive {
		// Detached: release the handle and exit. The VM keeps running.
		if err := machine.Close(); err != nil {
			log.Printf("[vmrunner] close handle: %v", err)
		}
		return
	}

	// Interactive mode: connect console, shut down VM on exit.
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

	if err := machine.InteractiveShell(cfg.PipeName); err != nil {
		log.Printf("[vmrunner] interactive shell ended: %v", err)
	}

	if err := machine.Shutdown(); err != nil {
		log.Printf("[vmrunner] shutdown error: %v", err)
	}
}

// cmdExec runs a command inside a VM via the serial console.
// If the VM is not already running it is started and left running after the
// command completes (detached).
func cmdExec(args []string) {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	f := addRunFlags(fs)
	trace := fs.Bool("trace", false, "") // hidden; superset of -debug
	_ = fs.Parse(args)

	if *trace {
		vm.Trace = true
		f.debug = true
	}

	cmdArgs := fs.Args()
	if len(cmdArgs) == 0 {
		log.Fatal("exec: command required\nusage: vmrunner exec [flags] <cmd> [args...]")
	}

	cfg := f.vmConfig()

	if f.debug {
		j, err := config.BuildJSON(cfg)
		if err != nil {
			log.Fatalf("config build error: %v", err)
		}
		log.Printf("[vmrunner] HCS config JSON:\n%s", j)
	}

	if err := vm.Exec(cfg, cmdArgs); err != nil {
		log.Fatalf("exec: %v", err)
	}
}

func cmdList() {
	if err := vm.List(); err != nil {
		log.Fatalf("list: %v", err)
	}
}

func cmdAttach(args []string) {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	_ = fs.Parse(args)

	if fs.NArg() < 1 {
		log.Fatal("attach: VM ID required\nusage: vmrunner attach <vm-id>")
	}
	id := fs.Arg(0)
	if err := vm.Attach(id); err != nil {
		log.Fatalf("attach %q: %v", id, err)
	}
}

func cmdStop(args []string) {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	_ = fs.Parse(args)

	if fs.NArg() < 1 {
		log.Fatal("stop: VM ID required\nusage: vmrunner stop <vm-id>")
	}
	id := fs.Arg(0)
	if err := vm.Stop(id); err != nil {
		log.Fatalf("stop %q: %v", id, err)
	}
	log.Printf("[vmrunner] VM %q stopped", id)
}

func cmdKill(args []string) {
	fs := flag.NewFlagSet("kill", flag.ExitOnError)
	_ = fs.Parse(args)

	if fs.NArg() < 1 {
		log.Fatal("kill: VM ID required\nusage: vmrunner kill <vm-id>")
	}
	id := fs.Arg(0)
	if err := vm.Kill(id); err != nil {
		log.Fatalf("kill %q: %v", id, err)
	}
	log.Printf("[vmrunner] VM %q terminated", id)
}
