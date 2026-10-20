package perf

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Shapes captured from `amd-smi metric --json -m -u -t -p -f` and
// `amd-smi static --json` on a Strix Halo APU.
const amdSmiApuMetricJSON = `{
  "gpu_data": [
    {
      "gpu": 0,
      "usage": {
        "gfx_activity": {"value": 42, "unit": "%"},
        "umc_activity": "N/A"
      },
      "temperature": {
        "edge": {"value": 64, "unit": "C"},
        "hotspot": "N/A",
        "mem": "N/A"
      },
      "power": {
        "socket_power": "N/A",
        "apu_average_socket_power": {"value": 31.5, "unit": "W"}
      },
      "fan": {"apu_fan_pwm": "N/A"},
      "mem_usage": {
        "total_vram": {"value": 2048, "unit": "MB"},
        "used_vram": {"value": 148, "unit": "MB"},
        "free_vram": {"value": 1900, "unit": "MB"},
        "total_gtt": {"value": 126976, "unit": "MB"},
        "used_gtt": {"value": 79606, "unit": "MB"}
      }
    }
  ]
}`

const amdSmiApuStaticJSON = `{
  "gpu_data": [
    {
      "gpu": 0,
      "asic": {
        "market_name": "AMD Radeon 8060S Graphics",
        "vendor_id": "0x1002",
        "target_graphics_version": "gfx1151"
      },
      "bus": {"bdf": "0000:c6:00.0"}
    }
  ]
}`

func TestParseAmdSmi_ApuUsesGTTForUnifiedMemory(t *testing.T) {
	stats := ParseAmdSmi([]byte(amdSmiApuMetricJSON), []byte(amdSmiApuStaticJSON))

	require.Len(t, stats, 1)
	stat := stats[0]

	assert.Equal(t, 0, stat.ID)
	assert.Equal(t, "AMD Radeon 8060S Graphics", stat.Name)
	assert.Equal(t, 42.0, stat.GpuUtilPct)
	assert.Equal(t, 64, stat.TempC)
	assert.Equal(t, 0, stat.VramTempC)
	assert.Equal(t, 31.5, stat.PowerDrawW)
	assert.Equal(t, 0.0, stat.FanSpeedPct)

	// The 2 GB VRAM carveout is not the pool the GPU works out of; GTT is.
	assert.Equal(t, 126976, stat.MemTotalMB)
	assert.Equal(t, 79606, stat.MemUsedMB)
	assert.InDelta(t, 79606.0/126976.0*100.0, stat.MemUtilPct, 0.001)
}

func TestParseAmdSmi_DiscreteUsesVRAM(t *testing.T) {
	metric := `{
	  "gpu_data": [
	    {
	      "gpu": 0,
	      "usage": {"gfx_activity": {"value": 87, "unit": "%"}},
	      "temperature": {
	        "edge": {"value": 71, "unit": "C"},
	        "mem": {"value": 83, "unit": "C"}
	      },
	      "power": {"socket_power": {"value": 212, "unit": "W"}},
	      "fan": {"fan_speed": {"value": 45, "unit": "%"}},
	      "mem_usage": {
	        "total_vram": {"value": 24576, "unit": "MB"},
	        "used_vram": {"value": 6144, "unit": "MB"},
	        "total_gtt": {"value": 8192, "unit": "MB"},
	        "used_gtt": {"value": 1024, "unit": "MB"}
	      }
	    }
	  ]
	}`
	static := `{"gpu_data": [{"gpu": 0, "asic": {"market_name": "AMD Radeon RX 7900 XTX"}}]}`

	stats := ParseAmdSmi([]byte(metric), []byte(static))

	require.Len(t, stats, 1)
	stat := stats[0]

	assert.Equal(t, "AMD Radeon RX 7900 XTX", stat.Name)
	assert.Equal(t, 87.0, stat.GpuUtilPct)
	assert.Equal(t, 71, stat.TempC)
	assert.Equal(t, 83, stat.VramTempC)
	assert.Equal(t, 212.0, stat.PowerDrawW)
	assert.Equal(t, 45.0, stat.FanSpeedPct)
	assert.Equal(t, 24576, stat.MemTotalMB)
	assert.Equal(t, 6144, stat.MemUsedMB)
	assert.Equal(t, 25.0, stat.MemUtilPct)
}

func TestParseAmdSmi_SmallVRAMWithoutLargerGTTStaysOnVRAM(t *testing.T) {
	metric := `{
	  "gpu_data": [
	    {
	      "gpu": 0,
	      "usage": {"gfx_activity": {"value": 1, "unit": "%"}},
	      "mem_usage": {
	        "total_vram": {"value": 2048, "unit": "MB"},
	        "used_vram": {"value": 512, "unit": "MB"},
	        "total_gtt": {"value": 1024, "unit": "MB"},
	        "used_gtt": {"value": 0, "unit": "MB"}
	      }
	    }
	  ]
	}`

	stats := ParseAmdSmi([]byte(metric), nil)

	require.Len(t, stats, 1)
	assert.Equal(t, 2048, stats[0].MemTotalMB)
	assert.Equal(t, 512, stats[0].MemUsedMB)
	assert.Equal(t, 25.0, stats[0].MemUtilPct)
	assert.Equal(t, "AMD GPU 0", stats[0].Name)
}

func TestParseAmdSmi_NAValuesDoNotPanic(t *testing.T) {
	metric := `{
	  "gpu_data": [
	    {
	      "gpu": 0,
	      "usage": {"gfx_activity": {"value": 0, "unit": "%"}},
	      "temperature": {"edge": "N/A", "mem": "N/A"},
	      "power": {"socket_power": "N/A"},
	      "fan": {"fan_speed": "N/A", "apu_fan_pwm": "N/A"},
	      "mem_usage": {
	        "total_vram": "N/A",
	        "used_vram": "N/A",
	        "total_gtt": "N/A",
	        "used_gtt": "N/A"
	      }
	    }
	  ]
	}`

	stats := ParseAmdSmi([]byte(metric), nil)

	require.Len(t, stats, 1)
	stat := stats[0]
	assert.Equal(t, 0, stat.TempC)
	assert.Equal(t, 0, stat.VramTempC)
	assert.Equal(t, 0.0, stat.PowerDrawW)
	assert.Equal(t, 0.0, stat.FanSpeedPct)
	assert.Equal(t, 0, stat.MemTotalMB)
	assert.Equal(t, 0, stat.MemUsedMB)
	assert.Equal(t, 0.0, stat.MemUtilPct)
}

func TestParseAmdSmi_MultipleGPUsAndSkipUnreadable(t *testing.T) {
	metric := `{
	  "gpu_data": [
	    {
	      "gpu": 0,
	      "usage": {"gfx_activity": {"value": 10, "unit": "%"}},
	      "mem_usage": {
	        "total_vram": {"value": 24576, "unit": "MB"},
	        "used_vram": {"value": 2048, "unit": "MB"}
	      }
	    },
	    {
	      "gpu": 1,
	      "usage": {"gfx_activity": {"value": 90, "unit": "%"}},
	      "mem_usage": {
	        "total_vram": {"value": 16384, "unit": "MB"},
	        "used_vram": {"value": 8192, "unit": "MB"}
	      }
	    },
	    {
	      "gpu": 2,
	      "usage": {"gfx_activity": "N/A"},
	      "temperature": {"edge": "N/A"},
	      "power": {"socket_power": "N/A"},
	      "fan": {"fan_speed": "N/A"},
	      "mem_usage": {"total_vram": "N/A"}
	    }
	  ]
	}`
	static := `{
	  "gpu_data": [
	    {"gpu": 0, "asic": {"market_name": "AMD Radeon RX 7900 XTX"}},
	    {"gpu": 1, "asic": {"market_name": "AMD Radeon RX 7900 XT"}}
	  ]
	}`

	stats := ParseAmdSmi([]byte(metric), []byte(static))

	require.Len(t, stats, 2)
	assert.Equal(t, "AMD Radeon RX 7900 XTX", stats[0].Name)
	assert.Equal(t, 10.0, stats[0].GpuUtilPct)
	assert.Equal(t, "AMD Radeon RX 7900 XT", stats[1].Name)
	assert.Equal(t, 1, stats[1].ID)
	assert.Equal(t, 50.0, stats[1].MemUtilPct)
}

func TestParseAmdSmi_StaticNameFallbacks(t *testing.T) {
	metric := `{
	  "gpu_data": [
	    {"gpu": 7, "usage": {"gfx_activity": {"value": 5, "unit": "%"}}},
	    {"gpu": 8, "usage": {"gfx_activity": {"value": 6, "unit": "%"}}}
	  ]
	}`

	t.Run("missing static document", func(t *testing.T) {
		stats := ParseAmdSmi([]byte(metric), nil)
		require.Len(t, stats, 2)
		assert.Equal(t, "AMD GPU 7", stats[0].Name)
		assert.Equal(t, "AMD GPU 8", stats[1].Name)
	})

	t.Run("malformed static document", func(t *testing.T) {
		stats := ParseAmdSmi([]byte(metric), []byte(`{"gpu_data": [`))
		require.Len(t, stats, 2)
		assert.Equal(t, "AMD GPU 7", stats[0].Name)
	})

	t.Run("N/A market name", func(t *testing.T) {
		static := `{"gpu_data": [{"gpu": 7, "asic": {"market_name": "N/A"}}]}`
		stats := ParseAmdSmi([]byte(metric), []byte(static))
		require.Len(t, stats, 2)
		assert.Equal(t, "AMD GPU 7", stats[0].Name)
	})

	t.Run("name keyed by id", func(t *testing.T) {
		static := `{"gpu_data": [{"gpu": 8, "asic": {"market_name": "AMD Radeon PRO W7900"}}]}`
		stats := ParseAmdSmi([]byte(metric), []byte(static))
		require.Len(t, stats, 2)
		assert.Equal(t, "AMD GPU 7", stats[0].Name)
		assert.Equal(t, "AMD Radeon PRO W7900", stats[1].Name)
	})
}

func TestParseAmdSmi_NumericFieldShapes(t *testing.T) {
	metric := `{
	  "gpu_data": [
	    {
	      "gpu": 3,
	      "usage": {"gfx_activity": 55},
	      "temperature": {"edge": 50, "mem": 60},
	      "power": {"socket_power": 100},
	      "fan": {"fan_speed": 30},
	      "mem_usage": {"total_vram": 8192, "used_vram": 4096}
	    }
	  ]
	}`

	stats := ParseAmdSmi([]byte(metric), nil)

	require.Len(t, stats, 1)
	stat := stats[0]
	assert.Equal(t, 3, stat.ID)
	assert.Equal(t, 55.0, stat.GpuUtilPct)
	assert.Equal(t, 50, stat.TempC)
	assert.Equal(t, 60, stat.VramTempC)
	assert.Equal(t, 100.0, stat.PowerDrawW)
	assert.Equal(t, 30.0, stat.FanSpeedPct)
	assert.Equal(t, 8192, stat.MemTotalMB)
	assert.Equal(t, 4096, stat.MemUsedMB)
	assert.Equal(t, 50.0, stat.MemUtilPct)
}

func TestParseAmdSmi_MalformedMetricJSON(t *testing.T) {
	assert.Empty(t, ParseAmdSmi([]byte(`{"gpu_data": [`), []byte(amdSmiApuStaticJSON)))
	assert.Empty(t, ParseAmdSmi(nil, nil))
	assert.Empty(t, ParseAmdSmi([]byte(`{"gpu_data": []}`), nil))
}

func TestParseAmdSmi_EmptyGPUEntriesAreSkipped(t *testing.T) {
	metric := `{"gpu_data": [{}, {"gpu": 1, "usage": {"gfx_activity": {"value": 1, "unit": "%"}}}]}`

	stats := ParseAmdSmi([]byte(metric), nil)

	require.Len(t, stats, 1)
	assert.Equal(t, 1, stats[0].ID)
}
