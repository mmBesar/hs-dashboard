package main

// gpu.go — finds graphics chips and reads usage, temperature and clock speed.
//
// Everything comes from /sys (no extra tools needed), so it works inside the
// scratch container. What each kind of GPU exposes:
//   AMD (amdgpu)         usage, temperature, clock, video memory
//   Intel (i915, xe)     clock, and a usage ESTIMATE from idle time (RC6 residency).
//                        Intel iGPUs have no temperature sensor of their own.
//   ARM Mali (devfreq)   usage if the kernel provides "load", clock, temperature
//                        from a "gpu" thermal zone
//   NVIDIA (proprietary) nothing in /sys, so it is not shown
// A GPU that gives no data at all is hidden.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type GPU struct {
	Name      string   `json:"name"`
	Driver    string   `json:"driver"`
	Usage     *float64 `json:"usage"` // percent, null when the driver gives none
	Temp      *float64 `json:"temp"`  // °C, null when there is no own sensor
	Freq      *float64 `json:"freq"`  // MHz
	VRAMUsed  uint64   `json:"vramUsed"`
	VRAMTotal uint64   `json:"vramTotal"`
	Estimated bool     `json:"estimated"` // usage is an estimate (Intel)
}

type gpuDev struct {
	key     string // real path of the device, used to avoid duplicates
	driver  string
	card    string // /sys/class/drm/cardN, empty if there is no DRM node
	dev     string // the device directory
	devfreq string // devfreq directory (Mali and friends), may be empty
}

// gpuState remembers the last idle-time reading so usage can be worked out as a difference.
type gpuState struct {
	rc6  uint64
	t    time.Time
	have bool
}

var cardRe = regexp.MustCompile(`^card\d+$`)

var gpuNames = map[string]string{
	"amdgpu": "AMD Radeon", "radeon": "AMD Radeon",
	"i915": "Intel Graphics", "xe": "Intel Graphics",
	"nouveau": "NVIDIA", "nvidia": "NVIDIA",
	"panfrost": "Mali GPU", "panthor": "Mali GPU", "lima": "Mali GPU", "mali": "Mali GPU",
	"msm": "Adreno GPU", "v3d": "VideoCore GPU", "etnaviv": "Vivante GPU", "asahi": "Apple GPU",
}

func firstGlob(pattern string) string {
	m, _ := filepath.Glob(pattern)
	if len(m) == 0 {
		return ""
	}
	return m[0]
}

func readFloat(path string) *float64 {
	if path == "" {
		return nil
	}
	v, err := strconv.ParseFloat(readStr(path), 64)
	if err != nil {
		return nil
	}
	return &v
}

func scaled(v *float64, div float64) *float64 {
	if v == nil {
		return nil
	}
	r := r1(*v / div)
	return &r
}

func discoverGPUs() []gpuDev {
	out := []gpuDev{}
	seen := map[string]bool{}

	cards, _ := filepath.Glob(sysDir + "/class/drm/card*")
	for _, c := range cards {
		if !cardRe.MatchString(filepath.Base(c)) {
			continue // skips connectors such as card0-HDMI-A-1
		}
		dev := c + "/device"
		real, err := filepath.EvalSymlinks(dev)
		if err != nil {
			continue
		}
		link, err := os.Readlink(dev + "/driver")
		if err != nil {
			continue
		}
		drv := filepath.Base(link)
		if _, known := gpuNames[drv]; !known {
			continue
		}
		out = append(out, gpuDev{key: real, driver: drv, card: c, dev: dev, devfreq: firstGlob(dev + "/devfreq/*")})
		seen[real] = true
	}

	// GPUs without a DRM node (Rockchip's vendor Mali driver, for example) only show up in devfreq.
	freqs, _ := filepath.Glob(sysDir + "/class/devfreq/*")
	for _, f := range freqs {
		n := strings.ToLower(filepath.Base(f))
		if !strings.Contains(n, "gpu") && !strings.Contains(n, "mali") {
			continue
		}
		real, err := filepath.EvalSymlinks(f)
		if err != nil {
			continue
		}
		parent := filepath.Dir(filepath.Dir(real))
		if seen[parent] {
			continue
		}
		out = append(out, gpuDev{key: parent, driver: "mali", dev: parent, devfreq: f})
		seen[parent] = true
	}
	return out
}

// gpuZoneTemp finds a thermal zone that belongs to the GPU (common on ARM boards).
func gpuZoneTemp() *float64 {
	zones, _ := filepath.Glob(sysDir + "/class/thermal/thermal_zone*")
	for _, z := range zones {
		if strings.Contains(strings.ToLower(readStr(z+"/type")), "gpu") {
			if v := readFloat(z + "/temp"); v != nil && *v > 0 && *v < 150000 {
				return scaled(v, 1000)
			}
		}
	}
	return nil
}

func hwmonValue(dev, file string) *float64 {
	return readFloat(firstGlob(dev + "/hwmon/hwmon*/" + file))
}

// read returns the current numbers, and false if this GPU exposes nothing useful.
func (d gpuDev) read(st *gpuState, now time.Time) (GPU, bool) {
	g := GPU{Name: gpuNames[d.driver], Driver: d.driver}

	switch d.driver {
	case "amdgpu", "radeon":
		g.Usage = readFloat(d.dev + "/gpu_busy_percent")
		g.Temp = scaled(hwmonValue(d.dev, "temp1_input"), 1000)
		g.Freq = scaled(hwmonValue(d.dev, "freq1_input"), 1e6) // Hz -> MHz
		if t := readFloat(d.dev + "/mem_info_vram_total"); t != nil {
			g.VRAMTotal = uint64(*t)
			if u := readFloat(d.dev + "/mem_info_vram_used"); u != nil {
				g.VRAMUsed = uint64(*u)
			}
		}

	case "i915", "xe":
		var rc6, freq string
		if d.driver == "i915" {
			rc6 = firstExisting(d.card+"/gt/gt0/rc6_residency_ms", d.card+"/power/rc6_residency_ms")
			freq = firstExisting(d.card+"/gt/gt0/rps_act_freq_mhz", d.card+"/gt_act_freq_mhz", d.card+"/gt_cur_freq_mhz")
		} else {
			rc6 = firstExisting(d.dev + "/tile0/gt0/gtidle/idle_residency_ms")
			freq = firstExisting(d.dev + "/tile0/gt0/freq0/act_freq")
		}
		g.Freq = readFloat(freq)
		if v := readFloat(rc6); v != nil {
			cur := uint64(*v)
			if st.have && cur >= st.rc6 {
				if dt := now.Sub(st.t).Milliseconds(); dt > 0 {
					busy := 100 * (1 - float64(cur-st.rc6)/float64(dt))
					busy = r1(clamp(busy, 0, 100))
					g.Usage, g.Estimated = &busy, true
				}
			}
			st.rc6, st.t, st.have = cur, now, true
		}

	default: // Mali and other ARM GPUs
		if d.devfreq != "" {
			if load := readStr(d.devfreq + "/load"); load != "" { // looks like "12@300000000Hz"
				if n, _, ok := strings.Cut(load, "@"); ok {
					if v, err := strconv.ParseFloat(n, 64); err == nil {
						g.Usage = &v
					}
				}
			}
			g.Freq = scaled(readFloat(d.devfreq+"/cur_freq"), 1e6)
		}
		if g.Usage == nil {
			g.Usage = readFloat(d.dev + "/utilisation") // vendor Mali kbase driver
		}
		g.Temp = gpuZoneTemp()
	}

	return g, g.Usage != nil || g.Temp != nil || g.Freq != nil
}

func firstExisting(paths ...string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
