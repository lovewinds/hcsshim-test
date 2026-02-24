//go:build windows

package vm

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/microsoft/hcsshim/vmrunner/internal/config"
	"github.com/microsoft/hcsshim/vmrunner/internal/vmcompute"
)

// VM wraps an HCS compute system handle and its configuration.
type VM struct {
	id     string
	system vmcompute.HcsSystem
	cfg    config.VMConfig
}

// Start creates and starts a new VM. It first cleans up any existing VM with
// the same ID to avoid "already exists" errors.
func Start(cfg config.VMConfig) (*VM, error) {
	// Clean up any pre-existing VM with the same ID.
	if err := cleanup(cfg.VMID); err != nil {
		log.Printf("[vmrunner] cleanup of existing VM %q: %v", cfg.VMID, err)
	}

	configJSON, err := config.BuildJSON(cfg)
	if err != nil {
		return nil, fmt.Errorf("build config: %w", err)
	}

	log.Printf("[vmrunner] creating VM %q", cfg.VMID)
	system, err := vmcompute.HcsCreateComputeSystem(cfg.VMID, configJSON)
	if err != nil {
		return nil, fmt.Errorf("HcsCreateComputeSystem: %w", err)
	}

	log.Printf("[vmrunner] starting VM %q", cfg.VMID)
	if err := vmcompute.HcsStartComputeSystem(system, ""); err != nil {
		_ = vmcompute.HcsCloseComputeSystem(system)
		return nil, fmt.Errorf("HcsStartComputeSystem: %w", err)
	}

	return &VM{id: cfg.VMID, system: system, cfg: cfg}, nil
}

// System returns the underlying HCS system handle.
func (v *VM) System() vmcompute.HcsSystem {
	return v.system
}

// ID returns the VM identifier.
func (v *VM) ID() string {
	return v.id
}

// Shutdown attempts a graceful shutdown, falling back to terminate.
func (v *VM) Shutdown() error {
	log.Printf("[vmrunner] shutting down VM %q", v.id)
	err := vmcompute.HcsShutdownComputeSystem(v.system, "")
	if err != nil {
		log.Printf("[vmrunner] graceful shutdown failed (%v), terminating", err)
		if termErr := vmcompute.HcsTerminateComputeSystem(v.system, ""); termErr != nil {
			// Close the handle even if terminate fails.
			_ = vmcompute.HcsCloseComputeSystem(v.system)
			return fmt.Errorf("terminate: %w", termErr)
		}
	}

	// Give the system a moment to stop before closing the handle.
	time.Sleep(500 * time.Millisecond)
	if err := vmcompute.HcsCloseComputeSystem(v.system); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}

// Close releases the system handle without shutting down the VM.
// The VM continues running in the background, managed by HCS.
func (v *VM) Close() error {
	return vmcompute.HcsCloseComputeSystem(v.system)
}

// Kill opens a VM by ID and forcibly terminates it, then closes the handle.
// It returns an error if the VM cannot be found or terminated.
func Kill(id string) error {
	system, err := vmcompute.HcsOpenComputeSystem(id)
	if err != nil {
		return fmt.Errorf("open VM %q: %w", id, err)
	}
	log.Printf("[vmrunner] killing VM %q", id)
	if err := vmcompute.HcsTerminateComputeSystem(system, ""); err != nil {
		_ = vmcompute.HcsCloseComputeSystem(system)
		return fmt.Errorf("terminate VM %q: %w", id, err)
	}
	return vmcompute.HcsCloseComputeSystem(system)
}

// cleanup opens and terminates an existing VM with the given ID, then closes its handle.
func cleanup(id string) error {
	system, err := vmcompute.HcsOpenComputeSystem(id)
	if err != nil {
		// Not found or cannot open – nothing to clean up.
		return nil
	}
	log.Printf("[vmrunner] found existing VM %q, cleaning up", id)
	_ = vmcompute.HcsTerminateComputeSystem(system, "")
	time.Sleep(300 * time.Millisecond)
	return vmcompute.HcsCloseComputeSystem(system)
}

// List enumerates all running compute systems and prints a formatted table.
func List() error {
	result, err := vmcompute.HcsEnumerateComputeSystems("")
	if err != nil {
		return fmt.Errorf("enumerate compute systems: %w", err)
	}
	if result == "" || result == "null" {
		fmt.Println("(no running VMs)")
		return nil
	}

	var items []struct {
		Id         string `json:"Id"`
		RuntimeId  string `json:"RuntimeId"`
		Owner      string `json:"Owner"`
		SystemType string `json:"SystemType"`
		State      string `json:"State"`
	}
	if err := json.Unmarshal([]byte(result), &items); err != nil {
		fmt.Println(result)
		return nil
	}
	if len(items) == 0 {
		fmt.Println("(no running VMs)")
		return nil
	}

	fmt.Printf("%-16s  %-16s  %-10s  %-24s  %s\n", "OWNER", "SYSTEMTYPE", "STATE", "NAME", "ID")
	fmt.Printf("%-16s  %-16s  %-10s  %-24s  %s\n",
		"----------------", "----------------", "----------", "------------------------", "------------------------------------")
	for _, item := range items {
		fmt.Printf("%-16s  %-16s  %-10s  %-24s  %s\n",
			item.Owner, item.SystemType, item.State, item.Id, item.RuntimeId)
	}
	return nil
}

// Stop opens a VM by ID and requests a graceful shutdown.
// Unlike Shutdown(), it does not fall back to terminate on failure.
func Stop(id string) error {
	system, err := vmcompute.HcsOpenComputeSystem(id)
	if err != nil {
		return fmt.Errorf("open VM %q: %w", id, err)
	}
	log.Printf("[vmrunner] stopping VM %q", id)
	if err := vmcompute.HcsShutdownComputeSystem(system, ""); err != nil {
		_ = vmcompute.HcsCloseComputeSystem(system)
		return fmt.Errorf("shutdown VM %q: %w", id, err)
	}
	time.Sleep(500 * time.Millisecond)
	return vmcompute.HcsCloseComputeSystem(system)
}

// Attach connects to the serial console of a running VM identified by id.
// It verifies the VM exists, then opens the named pipe console and connects
// it to the terminal bidirectionally.
func Attach(id string) error {
	system, err := vmcompute.HcsOpenComputeSystem(id)
	if err != nil {
		return fmt.Errorf("VM %q not found: %w", id, err)
	}
	_ = vmcompute.HcsCloseComputeSystem(system)

	pipeName := fmt.Sprintf(`\\.\pipe\%s-console`, id)
	v := &VM{id: id}
	return v.InteractiveShell(pipeName)
}

// Exec runs args in the VM identified by cfg.VMID via the serial console.
// If the VM is not already running it is started using cfg and left running
// (detached) after the command completes.
func Exec(cfg config.VMConfig, args []string) error {
	pipeName := fmt.Sprintf(`\\.\pipe\%s-console`, cfg.VMID)

	// Check if VM is already running.
	system, err := vmcompute.HcsOpenComputeSystem(cfg.VMID)
	if err == nil {
		// VM exists; release the extra open handle and use the serial console.
		_ = vmcompute.HcsCloseComputeSystem(system)
		v := &VM{id: cfg.VMID}
		return v.RunCommand(pipeName, args)
	}

	// VM not running; start it.
	log.Printf("[vmrunner] VM %q not running, starting...", cfg.VMID)
	machine, err := Start(cfg)
	if err != nil {
		return fmt.Errorf("start VM: %w", err)
	}

	runErr := machine.RunCommand(pipeName, args)

	// Detach: release the handle without shutting down the VM.
	_ = vmcompute.HcsCloseComputeSystem(machine.system)
	return runErr
}
