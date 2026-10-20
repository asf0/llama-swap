package perf

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// amdSmiDoc is the common envelope for `amd-smi metric --json` and
// `amd-smi static --json`. Entries are decoded as loose maps because amd-smi
// mixes objects, arrays, plain numbers, and the string "N/A" in the same
// document.
type amdSmiDoc struct {
	GPUData []map[string]any `json:"gpu_data"`
}

// ParseAmdSmi maps `amd-smi metric --json` and `amd-smi static --json` output
// to GpuStats. staticJSON may be nil or malformed, in which case devices keep
// the fallback name "AMD GPU <id>". A malformed metric document yields no
// stats.
func ParseAmdSmi(metricJSON, staticJSON []byte) []GpuStat {
	var metric amdSmiDoc
	if err := json.Unmarshal(metricJSON, &metric); err != nil {
		return nil
	}

	names := parseAmdSmiNames(staticJSON)

	stats := make([]GpuStat, 0, len(metric.GPUData))
	for i, gpu := range metric.GPUData {
		stat, ok := parseAmdSmiGPU(gpu, i)
		if !ok {
			continue
		}
		if name, ok := names[stat.ID]; ok {
			stat.Name = name
		}
		stats = append(stats, stat)
	}

	if len(stats) == 0 {
		return nil
	}
	return stats
}

// parseAmdSmiNames returns static market names keyed by amd-smi device index.
func parseAmdSmiNames(staticJSON []byte) map[int]string {
	names := make(map[int]string)
	if len(staticJSON) == 0 {
		return names
	}

	var static amdSmiDoc
	if err := json.Unmarshal(staticJSON, &static); err != nil {
		return names
	}

	for i, gpu := range static.GPUData {
		id := i
		if v, ok := amdSmiNumber(gpu["gpu"]); ok {
			id = int(v)
		}

		name, _ := amdSmiMap(gpu["asic"])["market_name"].(string)
		name = strings.TrimSpace(name)
		if name == "" || strings.EqualFold(name, "N/A") {
			continue
		}
		names[id] = name
	}

	return names
}

// parseAmdSmiGPU converts one gpu_data entry. It reports false only when no
// usable metric could be read, so devices with partial data are still shown.
func parseAmdSmiGPU(gpu map[string]any, index int) (GpuStat, bool) {
	id := index
	if v, ok := amdSmiNumber(gpu["gpu"]); ok {
		id = int(v)
	}

	stat := GpuStat{
		Timestamp: time.Now(),
		ID:        id,
		Name:      fmt.Sprintf("AMD GPU %d", id),
	}
	usable := false

	usage := amdSmiMap(gpu["usage"])
	if v, ok := amdSmiNumber(usage["gfx_activity"]); ok {
		stat.GpuUtilPct = v
		usable = true
	}

	temps := amdSmiMap(gpu["temperature"])
	if v, ok := amdSmiNumber(temps["edge"]); ok {
		stat.TempC = int(math.Round(v))
		usable = true
	}
	if v, ok := amdSmiNumber(temps["mem"]); ok {
		stat.VramTempC = int(math.Round(v))
		usable = true
	}

	power := amdSmiMap(gpu["power"])
	if v, ok := amdSmiNumber(power["socket_power"]); ok && v > 0 {
		stat.PowerDrawW = v
		usable = true
	} else if v, ok := amdSmiNumber(power["apu_average_socket_power"]); ok {
		stat.PowerDrawW = v
		usable = true
	}

	fan := amdSmiMap(gpu["fan"])
	if v, ok := amdSmiNumber(fan["fan_speed"]); ok {
		stat.FanSpeedPct = v
		usable = true
	} else if v, ok := amdSmiNumber(fan["apu_fan_pwm"]); ok {
		stat.FanSpeedPct = v
		usable = true
	}

	mem := amdSmiMap(gpu["mem_usage"])
	totalVRAM, hasVRAM := amdSmiNumber(mem["total_vram"])
	usedVRAM, _ := amdSmiNumber(mem["used_vram"])
	totalGTT, _ := amdSmiNumber(mem["total_gtt"])
	usedGTT, _ := amdSmiNumber(mem["used_gtt"])

	totalMB, usedMB := totalVRAM, usedVRAM
	// APUs such as Strix Halo expose only a small VRAM carveout (2 GB), while
	// the GPU really works out of the much larger GTT pool. Discrete GPUs
	// keep their dedicated VRAM.
	if hasVRAM && totalVRAM > 0 && totalVRAM <= 4096 && totalGTT > totalVRAM {
		totalMB, usedMB = totalGTT, usedGTT
	}

	if totalMB > 0 {
		stat.MemTotalMB = int(totalMB)
		stat.MemUsedMB = int(usedMB)
		stat.MemUtilPct = float64(stat.MemUsedMB) / float64(stat.MemTotalMB) * 100
		usable = true
	}

	if !usable {
		return GpuStat{}, false
	}
	return stat, true
}

// amdSmiMap returns v as a map when amd-smi used an object shape.
func amdSmiMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// amdSmiNumber accepts a plain number, a stringified number, or an object such
// as {"value": 42, "unit": "%"}. "N/A", empty strings, and missing values are
// reported as not usable.
func amdSmiNumber(v any) (float64, bool) {
	switch value := v.(type) {
	case float64:
		return value, true
	case json.Number:
		f, err := value.Float64()
		return f, err == nil
	case string:
		value = strings.TrimSpace(value)
		if value == "" || strings.EqualFold(value, "N/A") {
			return 0, false
		}
		f, err := strconv.ParseFloat(value, 64)
		return f, err == nil
	case map[string]any:
		return amdSmiNumber(value["value"])
	}
	return 0, false
}
