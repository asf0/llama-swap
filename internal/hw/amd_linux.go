//go:build linux

package hw

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func detectAMD(ctx context.Context) []detectedAccelerator {
	var result []detectedAccelerator
	if rocm, err := detectROCm(ctx); err == nil {
		result = append(result, rocm...)
	}
	// KFD runs first because it reports the authoritative architecture even
	// when no vendor tool is installed. amd-smi then fills in the market
	// name, memory model, driver, and power limit that KFD cannot provide.
	if kfd, err := detectAMDKFD(
		"/sys/class/kfd/kfd/topology/nodes",
		"/sys/class/drm",
		"/dev/dri",
	); err == nil {
		result = append(result, kfd...)
	}
	if _, err := exec.LookPath("rocm-smi"); err != nil {
		if smi, err := detectAmdSmi(ctx); err == nil {
			result = append(result, smi...)
		}
	}
	return result
}

// amdSmiCommandTimeout bounds each amd-smi invocation. Static and metric
// queries are separate commands, so a slow metric query cannot block the
// static fields.
const amdSmiCommandTimeout = 5 * time.Second

// amdSmiDocument is the gpu_data envelope shared by `amd-smi static --json`
// and `amd-smi metric --json`. Entries stay as loose maps because amd-smi
// mixes objects, numbers, and the string "N/A".
type amdSmiDocument struct {
	GPUData []map[string]any `json:"gpu_data"`
}

// detectAmdSmi reads static identity and memory facts from amd-smi. It is the
// fallback when rocm-smi is unavailable; the metric query is optional because
// static output still supplies the model, architecture, and driver.
func detectAmdSmi(ctx context.Context) ([]detectedAccelerator, error) {
	if _, err := exec.LookPath("amd-smi"); err != nil {
		return nil, err
	}

	staticJSON, err := runAmdSmi(ctx, "static", "--json")
	if err != nil {
		return nil, fmt.Errorf("querying amd-smi static: %w", err)
	}
	// A failed metric query leaves memory unknown without dropping the
	// static record.
	metricJSON, _ := runAmdSmi(ctx, "metric", "--json", "-m")

	return parseAmdSmiDevices(staticJSON, metricJSON)
}

func runAmdSmi(ctx context.Context, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, amdSmiCommandTimeout)
	defer cancel()
	return exec.CommandContext(commandCtx, "amd-smi", args...).Output()
}

// parseAmdSmiDevices converts static and metric documents into accelerator
// records. A missing or malformed metric document is not an error: the static
// fields are still reported.
func parseAmdSmiDevices(staticJSON, metricJSON []byte) ([]detectedAccelerator, error) {
	var static amdSmiDocument
	if err := json.Unmarshal(staticJSON, &static); err != nil {
		return nil, fmt.Errorf("parsing amd-smi static output: %w", err)
	}
	metrics := amdSmiMetrics(metricJSON)

	result := make([]detectedAccelerator, 0, len(static.GPUData))
	for index, gpu := range static.GPUData {
		identity := normalizePCIIdentity(amdSmiString(amdSmiMap(gpu["bus"])["bdf"]))
		if identity == "" {
			continue
		}
		asic := amdSmiMap(gpu["asic"])

		var driver *Driver
		driverInfo := amdSmiMap(gpu["driver"])
		driverName := nonEmptyStringPtr(amdSmiString(driverInfo["name"]))
		driverVersion := nonEmptyStringPtr(amdSmiString(driverInfo["version"]))
		if driverName != nil || driverVersion != nil {
			driver = &Driver{Name: driverName, Version: driverVersion}
		}

		var powerLimit *float64
		powerInfo := amdSmiMap(amdSmiMap(gpu["limit"])["ppt0"])
		for _, key := range []string{"socket_power_limit", "max_power_limit"} {
			if watts, ok := amdSmiNumber(powerInfo[key]); ok && watts > 0 {
				powerLimit = float64Ptr(watts)
				break
			}
		}

		deviceIndex := index
		if id, ok := amdSmiNumber(gpu["gpu"]); ok {
			deviceIndex = int(id)
		}
		memory := amdSmiMemory(metrics[deviceIndex])

		result = append(result, detectedAccelerator{
			identity: identity,
			value: Accelerator{
				Kind:            "gpu",
				Vendor:          stringPtr("AMD"),
				Model:           nonEmptyStringPtr(amdSmiString(asic["market_name"])),
				Architecture:    nonEmptyStringPtr(amdSmiString(asic["target_graphics_version"])),
				Memory:          memory,
				Driver:          driver,
				PowerLimitWatts: powerLimit,
			},
		})
	}
	return result, nil
}

// amdSmiMetrics indexes metric entries by the amd-smi device id, falling back
// to their position when the id is missing.
func amdSmiMetrics(metricJSON []byte) map[int]map[string]any {
	metrics := make(map[int]map[string]any)
	if len(metricJSON) == 0 {
		return metrics
	}
	var metric amdSmiDocument
	if err := json.Unmarshal(metricJSON, &metric); err != nil {
		return metrics
	}
	for index, gpu := range metric.GPUData {
		id := index
		if parsed, ok := amdSmiNumber(gpu["gpu"]); ok {
			id = int(parsed)
		}
		metrics[id] = gpu
	}
	return metrics
}

// amdSmiMemory applies the APU memory rule: a small VRAM carveout with a
// larger GTT pool describes unified memory, while a discrete card owns its
// VRAM. Unknown data stays unknown rather than inventing a capacity.
func amdSmiMemory(gpu map[string]any) AcceleratorMemory {
	memUsage := amdSmiMap(gpu["mem_usage"])
	vramMB, hasVRAM := amdSmiNumberMB(memUsage["total_vram"])
	gttMB, hasGTT := amdSmiNumberMB(memUsage["total_gtt"])

	switch {
	case hasVRAM && vramMB > 0 && vramMB <= 4096 && hasGTT && gttMB > vramMB:
		return AcceleratorMemory{Kind: "unified", CapacityBytes: uint64Ptr(mbToBytes(gttMB))}
	case hasVRAM && vramMB > 0:
		return AcceleratorMemory{Kind: "dedicated", CapacityBytes: uint64Ptr(mbToBytes(vramMB))}
	case hasGTT && gttMB > 0:
		// Without a VRAM carveout the GTT pool is the only GPU-visible
		// memory, which describes unified memory.
		return AcceleratorMemory{Kind: "unified", CapacityBytes: uint64Ptr(mbToBytes(gttMB))}
	default:
		return AcceleratorMemory{Kind: "unknown"}
	}
}

func mbToBytes(megabytes float64) uint64 {
	if megabytes <= 0 {
		return 0
	}
	return uint64(math.Round(megabytes * 1024 * 1024))
}

// amdSmiNumberMB converts an amd-smi memory value to megabytes. Values
// without a unit default to MB because amd-smi mem_usage reports MB.
func amdSmiNumberMB(value any) (float64, bool) {
	number, unit, ok := amdSmiValue(value)
	if !ok || number < 0 {
		return 0, false
	}
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "", "mb", "mib":
		return number, true
	case "kb", "kib":
		return number / 1024, true
	case "gb", "gib":
		return number * 1024, true
	case "b", "byte", "bytes":
		return number / (1024 * 1024), true
	default:
		return 0, false
	}
}

// amdSmiValue accepts a plain number, a numeric string, or an object such as
// {"value": 42, "unit": "MB"}. "N/A" and missing values are unusable.
func amdSmiValue(value any) (float64, string, bool) {
	switch typed := value.(type) {
	case map[string]any:
		number, ok := amdSmiNumber(typed["value"])
		if !ok {
			return 0, "", false
		}
		unit, _ := typed["unit"].(string)
		return number, unit, true
	default:
		number, ok := amdSmiNumber(typed)
		return number, "", ok
	}
}

// amdSmiNumber accepts the numeric shapes amd-smi uses, including a nested
// {"value": ...} object. "N/A", empty strings, and missing values fail.
func amdSmiNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	case string:
		text := strings.TrimSpace(typed)
		if text == "" || strings.EqualFold(text, "N/A") {
			return 0, false
		}
		number, err := strconv.ParseFloat(text, 64)
		return number, err == nil
	case map[string]any:
		return amdSmiNumber(typed["value"])
	}
	return 0, false
}

func amdSmiString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func amdSmiMap(value any) map[string]any {
	mapped, _ := value.(map[string]any)
	return mapped
}

func detectROCm(ctx context.Context) ([]detectedAccelerator, error) {
	if _, err := exec.LookPath("rocm-smi"); err != nil {
		return nil, err
	}
	args := []string{"-i", "--showmeminfo", "vram", "--showproductname", "--showdriverversion", "--showmaxpower", "--showbus", "--csv"}
	output, err := exec.CommandContext(ctx, "rocm-smi", args...).Output()
	if err != nil {
		fallback := []string{"-i", "--showmeminfo", "vram", "--showproductname", "--showbus", "--csv"}
		output, err = exec.CommandContext(ctx, "rocm-smi", fallback...).Output()
	}
	if err != nil {
		return nil, fmt.Errorf("querying rocm-smi: %w", err)
	}
	return parseROCmCSV(string(output))
}

func parseROCmCSV(output string) ([]detectedAccelerator, error) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	var header []string
	var result []detectedAccelerator
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		row, err := csv.NewReader(strings.NewReader(line)).Read()
		if err != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(row[0]), "device") {
			header = row
			continue
		}
		if len(header) == 0 || len(row) != len(header) {
			continue
		}
		fields := make(map[string]string, len(row))
		for i := range row {
			fields[strings.ToLower(strings.TrimSpace(header[i]))] = strings.TrimSpace(row[i])
		}
		model := firstROCmField(fields, "card series", "device name", "card model")
		architecture := firstROCmField(fields, "gfx version")
		identity := normalizePCIIdentity(firstROCmField(fields, "pci bus", "pci bus id", "bus"))
		if identity == "" {
			identity = firstROCmField(fields, "guid", "unique id")
		}
		memoryBytes, _ := strconv.ParseUint(firstROCmField(fields, "vram total memory (b)"), 10, 64)
		memory := AcceleratorMemory{Kind: "dedicated"}
		if memoryBytes > 0 {
			memory.CapacityBytes = uint64Ptr(memoryBytes)
		}
		driverVersion := firstROCmField(fields, "driver version")
		var driver *Driver
		if version := nonEmptyStringPtr(driverVersion); version != nil {
			driver = &Driver{Name: stringPtr("amdgpu"), Version: version}
		}
		power, _ := parseOptionalFloat(firstROCmField(fields, "max graphics package power (w)", "maximum graphics package power (w)"))
		accelerator := Accelerator{
			Kind:         "gpu",
			Vendor:       stringPtr("AMD"),
			Model:        nonEmptyStringPtr(model),
			Architecture: nonEmptyStringPtr(architecture),
			Memory:       memory,
			Driver:       driver,
		}
		if power > 0 {
			accelerator.PowerLimitWatts = float64Ptr(power)
		}
		result = append(result, detectedAccelerator{identity: identity, value: accelerator})
	}
	return result, scanner.Err()
}

func firstROCmField(fields map[string]string, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(fields[name]); value != "" && !strings.EqualFold(value, "n/a") {
			return value
		}
	}
	return ""
}

func detectAMDKFD(nodesRoot, drmRoot, deviceRoot string) ([]detectedAccelerator, error) {
	propertyPaths, err := filepath.Glob(filepath.Join(nodesRoot, "*", "properties"))
	if err != nil {
		return nil, err
	}

	var result []detectedAccelerator
	for _, propertyPath := range propertyPaths {
		properties, err := readKFDProperties(propertyPath)
		if err != nil {
			continue
		}
		architecture := formatGFXTarget(properties["gfx_target_version"])
		renderMinor := properties["drm_render_minor"]
		if architecture == "" || renderMinor == 0 {
			continue
		}

		renderNode := fmt.Sprintf("renderD%d", renderMinor)
		if _, err := os.Stat(filepath.Join(deviceRoot, renderNode)); err != nil {
			continue
		}
		devicePath, err := filepath.EvalSymlinks(filepath.Join(drmRoot, renderNode, "device"))
		if err != nil || pciVendorName(readTrimmed(filepath.Join(devicePath, "vendor"))) != "AMD" {
			continue
		}
		identity := normalizePCIIdentity(filepath.Base(devicePath))
		if identity == "" {
			continue
		}

		result = append(result, detectedAccelerator{
			identity: identity,
			value: Accelerator{
				Kind:         "gpu",
				Vendor:       stringPtr("AMD"),
				Architecture: stringPtr(architecture),
				Memory:       AcceleratorMemory{Kind: "unknown"},
			},
		})
	}
	return result, nil
}

func readKFDProperties(path string) (map[string]uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	properties := make(map[string]uint64)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err == nil {
			properties[fields[0]] = value
		}
	}
	return properties, scanner.Err()
}

func formatGFXTarget(version uint64) string {
	if version < 10000 {
		return ""
	}
	major := version / 10000
	minor := (version / 100) % 100
	stepping := version % 100
	if major == 0 || minor > 9 || stepping > 15 {
		return ""
	}
	return fmt.Sprintf("gfx%d%d%x", major, minor, stepping)
}
