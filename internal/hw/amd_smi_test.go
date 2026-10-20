//go:build linux

package hw

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures use the shapes captured from `amd-smi static --json` and
// `amd-smi metric --json -m` on a Strix Halo APU and a discrete Radeon.
const amdSmiAPUStatic = `{
  "gpu_data": [
    {
      "gpu": 0,
      "asic": {
        "market_name": "AMD Radeon 8060S Graphics",
        "target_graphics_version": "gfx1151"
      },
      "bus": {"bdf": "0000:c6:00.0"},
      "limit": {
        "ppt0": {
          "socket_power_limit": "N/A",
          "max_power_limit": "N/A"
        }
      },
      "driver": {"name": "amdgpu", "version": "N/A"}
    }
  ]
}`

const amdSmiAPUMetric = `{
  "gpu_data": [
    {
      "gpu": 0,
      "mem_usage": {
        "total_vram": {"value": 2048, "unit": "MB"},
        "total_gtt": {"value": 126976, "unit": "MB"}
      }
    }
  ]
}`

const amdSmiDiscreteStatic = `{
  "gpu_data": [
    {
      "gpu": 0,
      "asic": {
        "market_name": "Radeon RX 7900 XTX",
        "target_graphics_version": "gfx1100"
      },
      "bus": {"bdf": "0000:03:00.0"},
      "limit": {
        "ppt0": {
          "socket_power_limit": {"value": 339, "unit": "W"},
          "max_power_limit": "N/A"
        }
      },
      "driver": {"name": "amdgpu", "version": "6.12.12"}
    }
  ]
}`

const amdSmiDiscreteMetric = `{
  "gpu_data": [
    {
      "gpu": 0,
      "mem_usage": {
        "total_vram": {"value": 24576, "unit": "MB"},
        "total_gtt": {"value": 8192, "unit": "MB"}
      }
    }
  ]
}`

func TestHardware_ParseAmdSmiAPU(t *testing.T) {
	got, err := parseAmdSmiDevices([]byte(amdSmiAPUStatic), []byte(amdSmiAPUMetric))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("parseAmdSmiDevices() returned %d records", len(got))
	}
	accelerator := got[0].value
	if got[0].identity != "0000:c6:00.0" {
		t.Fatalf("identity = %q", got[0].identity)
	}
	if stringValue(accelerator.Model) != "AMD Radeon 8060S Graphics" {
		t.Fatalf("model = %q", stringValue(accelerator.Model))
	}
	if stringValue(accelerator.Architecture) != "gfx1151" {
		t.Fatalf("architecture = %q", stringValue(accelerator.Architecture))
	}
	if accelerator.Memory.Kind != "unified" {
		t.Fatalf("memory kind = %q, want unified", accelerator.Memory.Kind)
	}
	want := uint64(126976) * 1024 * 1024
	if accelerator.Memory.CapacityBytes == nil || *accelerator.Memory.CapacityBytes != want {
		t.Fatalf("memory capacity = %v, want %d", accelerator.Memory.CapacityBytes, want)
	}
	if stringValue(accelerator.Vendor) != "AMD" {
		t.Fatalf("vendor = %q", stringValue(accelerator.Vendor))
	}
	if accelerator.Driver == nil || stringValue(accelerator.Driver.Name) != "amdgpu" ||
		accelerator.Driver.Version != nil {
		t.Fatalf("driver = %+v, want name amdgpu with no version", accelerator.Driver)
	}
	if accelerator.PowerLimitWatts != nil {
		t.Fatalf("power limit = %v, want nil", accelerator.PowerLimitWatts)
	}
}

func TestHardware_ParseAmdSmiDiscrete(t *testing.T) {
	got, err := parseAmdSmiDevices([]byte(amdSmiDiscreteStatic), []byte(amdSmiDiscreteMetric))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("parseAmdSmiDevices() returned %d records", len(got))
	}
	accelerator := got[0].value
	if accelerator.Memory.Kind != "dedicated" {
		t.Fatalf("memory kind = %q, want dedicated", accelerator.Memory.Kind)
	}
	want := uint64(24576) * 1024 * 1024
	if accelerator.Memory.CapacityBytes == nil || *accelerator.Memory.CapacityBytes != want {
		t.Fatalf("memory capacity = %v, want %d", accelerator.Memory.CapacityBytes, want)
	}
	if accelerator.Driver == nil || stringValue(accelerator.Driver.Version) != "6.12.12" {
		t.Fatalf("driver = %+v, want version 6.12.12", accelerator.Driver)
	}
	if accelerator.PowerLimitWatts == nil || *accelerator.PowerLimitWatts != 339 {
		t.Fatalf("power limit = %v, want 339", accelerator.PowerLimitWatts)
	}
}

func TestHardware_ParseAmdSmiStaticOnly(t *testing.T) {
	for name, metric := range map[string][]byte{
		"missing":   nil,
		"malformed": []byte(`{"gpu_data": [`),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseAmdSmiDevices([]byte(amdSmiAPUStatic), metric)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("parseAmdSmiDevices() returned %d records", len(got))
			}
			accelerator := got[0].value
			if stringValue(accelerator.Model) != "AMD Radeon 8060S Graphics" ||
				stringValue(accelerator.Architecture) != "gfx1151" {
				t.Fatalf("static fields lost: %+v", accelerator)
			}
			if accelerator.Memory.Kind != "unknown" || accelerator.Memory.CapacityBytes != nil {
				t.Fatalf("memory = %+v, want unknown without metric data", accelerator.Memory)
			}
		})
	}
}

func TestHardware_ParseAmdSmiNAFields(t *testing.T) {
	got, err := parseAmdSmiDevices([]byte(amdSmiAPUStatic), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("parseAmdSmiDevices() returned %d records", len(got))
	}
	accelerator := got[0].value
	if accelerator.Driver == nil || stringValue(accelerator.Driver.Name) != "amdgpu" {
		t.Fatalf("driver = %+v, want amdgpu", accelerator.Driver)
	}
	if accelerator.Driver.Version != nil {
		t.Fatalf("driver version = %q, want nil for N/A", stringValue(accelerator.Driver.Version))
	}
	if accelerator.PowerLimitWatts != nil {
		t.Fatalf("power limit = %v, want nil for N/A", accelerator.PowerLimitWatts)
	}
}

func TestHardware_AmdSmiMergesWithKFD(t *testing.T) {
	smi, err := parseAmdSmiDevices([]byte(amdSmiAPUStatic), []byte(amdSmiAPUMetric))
	if err != nil {
		t.Fatal(err)
	}
	kfd := detectedAccelerator{
		identity: "0000:c6:00.0",
		value: Accelerator{
			Kind:         "gpu",
			Vendor:       stringPtr("AMD"),
			Architecture: stringPtr("gfx1151"),
			Memory:       AcceleratorMemory{Kind: "unknown"},
		},
	}
	got := finalizeAccelerators(append([]detectedAccelerator{kfd}, smi...))
	if len(got) != 1 {
		t.Fatalf("finalizeAccelerators() returned %d records, want 1", len(got))
	}
	want := uint64(126976) * 1024 * 1024
	if stringValue(got[0].Architecture) != "gfx1151" ||
		stringValue(got[0].Model) != "AMD Radeon 8060S Graphics" ||
		got[0].Memory.Kind != "unified" ||
		got[0].Memory.CapacityBytes == nil ||
		*got[0].Memory.CapacityBytes != want {
		t.Fatalf("merged accelerator = %+v", got[0])
	}
}

func TestHardware_DetectAmdSmiMissingTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := detectAmdSmi(context.Background()); err == nil {
		t.Fatal("detectAmdSmi() succeeded without amd-smi in PATH")
	}
}

func TestHardware_DetectAmdSmiRunsBothQueries(t *testing.T) {
	dir := t.TempDir()
	fixtures := filepath.Join(dir, "fixtures")
	bin := filepath.Join(dir, "bin")
	for _, path := range []string{fixtures, bin} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		"static.json": amdSmiAPUStatic,
		"metric.json": amdSmiAPUMetric,
	} {
		if err := os.WriteFile(filepath.Join(fixtures, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(dir, "commands.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$AMD_SMI_LOG"
case "$1" in
  static) cat "$AMD_SMI_FIXTURES/static.json" ;;
  metric) cat "$AMD_SMI_FIXTURES/metric.json" ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "amd-smi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMD_SMI_FIXTURES", fixtures)
	t.Setenv("AMD_SMI_LOG", logPath)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := detectAmdSmi(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || stringValue(got[0].value.Model) != "AMD Radeon 8060S Graphics" {
		t.Fatalf("detectAmdSmi() = %+v", got)
	}

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(logged)), "\n")
	if len(lines) != 2 || lines[0] != "static --json" || lines[1] != "metric --json -m" {
		t.Fatalf("amd-smi commands = %q", lines)
	}
}
