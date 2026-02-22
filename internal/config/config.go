//go:build windows

package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

// VMConfig holds user-facing VM configuration options.
type VMConfig struct {
	ImageDir    string
	MemoryMB    uint32
	CPUCount    uint32
	KernelArgs  string
	VMID        string
	PipeName    string
}

// --- HCS Schema2 JSON structures ---

type schemaVersion struct {
	Major int `json:"Major"`
	Minor int `json:"Minor"`
}

type linuxKernelDirect struct {
	KernelFilePath string `json:"KernelFilePath"`
	InitRdPath     string `json:"InitRdPath"`
	KernelCmdLine  string `json:"KernelCmdLine"`
}

type chipset struct {
	LinuxKernelDirect linuxKernelDirect `json:"LinuxKernelDirect"`
}

type memory struct {
	SizeInMB uint32 `json:"SizeInMB"`
}

type processor struct {
	Count uint32 `json:"Count"`
}

type computeTopology struct {
	Memory    memory    `json:"Memory"`
	Processor processor `json:"Processor"`
}

type scsiAttachment struct {
	Type string `json:"Type"`
	Path string `json:"Path"`
}

type scsiController struct {
	Attachments map[string]scsiAttachment `json:"Attachments"`
}

type comPort struct {
	NamedPipe string `json:"NamedPipe"`
}

type devices struct {
	Scsi     map[string]scsiController `json:"Scsi"`
	ComPorts map[string]comPort        `json:"ComPorts"`
}

type virtualMachine struct {
	Chipset         chipset         `json:"Chipset"`
	ComputeTopology computeTopology `json:"ComputeTopology"`
	Devices         devices         `json:"Devices"`
}

type hcsDocument struct {
	Owner         string        `json:"Owner"`
	SchemaVersion schemaVersion `json:"SchemaVersion"`
	VirtualMachine virtualMachine `json:"VirtualMachine"`
}

// DefaultKernelArgs is the default kernel command line.
const DefaultKernelArgs = "console=ttyS0 root=/dev/sda rw init=/sbin/init"

// BuildJSON constructs the HCS schema2 JSON configuration string for the VM.
func BuildJSON(cfg VMConfig) (string, error) {
	if cfg.ImageDir == "" {
		return "", fmt.Errorf("image directory must not be empty")
	}

	kernelArgs := cfg.KernelArgs
	if kernelArgs == "" {
		kernelArgs = DefaultKernelArgs
	}

	pipeName := cfg.PipeName
	if pipeName == "" {
		pipeName = fmt.Sprintf(`\\.\pipe\%s-console`, cfg.VMID)
	}

	// Windows paths use backslashes; filepath.Join on Linux produces forward
	// slashes, so we use a helper that always produces Windows-style paths.
	kernelPath := winPath(cfg.ImageDir, "vmlinuz")
	initrdPath := winPath(cfg.ImageDir, "initrd")
	vhdxPath   := winPath(cfg.ImageDir, "rootfs.vhdx")

	doc := hcsDocument{
		Owner: "vmrunner",
		SchemaVersion: schemaVersion{Major: 2, Minor: 1},
		VirtualMachine: virtualMachine{
			Chipset: chipset{
				LinuxKernelDirect: linuxKernelDirect{
					KernelFilePath: kernelPath,
					InitRdPath:     initrdPath,
					KernelCmdLine:  kernelArgs,
				},
			},
			ComputeTopology: computeTopology{
				Memory:    memory{SizeInMB: cfg.MemoryMB},
				Processor: processor{Count: cfg.CPUCount},
			},
			Devices: devices{
				Scsi: map[string]scsiController{
					"0": {
						Attachments: map[string]scsiAttachment{
							"0": {
								Type: "VirtualDisk",
								Path: vhdxPath,
							},
						},
					},
				},
				ComPorts: map[string]comPort{
					"0": {NamedPipe: pipeName},
				},
			},
		},
	}

	b, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal HCS config: %w", err)
	}
	return string(b), nil
}

// winPath joins a Windows-style base directory with a filename.
// cfg.ImageDir may already be a Windows path (e.g. "C:\source\...").
func winPath(dir, file string) string {
	// filepath.Join works correctly on Windows at runtime.
	// When cross-compiled on Linux, we still need correct Windows paths.
	// We rely on the caller to pass Windows-style dir (backslashes).
	// filepath.Join on Linux would use forward slashes, so we do it manually.
	if len(dir) > 0 && dir[len(dir)-1] == '\\' {
		return dir + file
	}
	// Try os-agnostic join first; if the dir looks like a Windows path, use backslash.
	if isWindowsPath(dir) {
		return dir + `\` + file
	}
	return filepath.Join(dir, file)
}

func isWindowsPath(path string) bool {
	// Detect drive letter or UNC path.
	if len(path) >= 2 && path[1] == ':' {
		return true
	}
	if len(path) >= 2 && path[0] == '\\' && path[1] == '\\' {
		return true
	}
	return false
}
