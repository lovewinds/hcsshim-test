//go:build windows

package vm

import (
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
