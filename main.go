// hs-dashboard — main.go
// Single binary dashboard server.
// Reads host metrics from /host/proc and /host/sys (mounted read-only).
// Serves static files embedded at compile time.
// Checks service URLs and serves results as JSON.
//
// github.com/mmBesar/hs-dashboard

package main

import (
	"context"
	"crypto/tls"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ── Embedded static files ─────────────────────────────────────────────────────
// All files in www/ are embedded into the binary at compile time.
// No external files needed at runtime.

//go:embed www
var staticFiles embed.FS

// ── Configuration ─────────────────────────────────────────────────────────────

type ServerConfig struct {
	Name        string `json:"name"`
	FQDN        string `json:"fqdn"`
	IP          string `json:"ip"`
	DNS         string `json:"dns"`
	Description string `json:"description"`
}

type HardwareConfig struct {
	Board   string `json:"board"`
	CPU     string `json:"cpu"`
	Arch    string `json:"arch"` // null = auto-detect
	RAM     string `json:"ram"`
	Storage string `json:"storage"`
	OS      string `json:"os"`
	Kernel  string `json:"kernel"`
}

type LogoConfig struct {
	Image  *string `json:"image"`
	Height int     `json:"height"`
}

type ThermalZone struct {
	Path        string `json:"path"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type ThermalConfig struct {
	Zones []ThermalZone `json:"zones"`
}

type DisplayConfig struct {
	Timezone       string  `json:"timezone"`
	Theme          string  `json:"theme"`
	Scale          float64 `json:"scale"`
	RefreshStatsMs int     `json:"refresh_stats_ms"`
	RefreshStatusMs int    `json:"refresh_status_ms"`
}

type DashboardConfig struct {
	Server   ServerConfig   `json:"server"`
	Hardware HardwareConfig `json:"hardware"`
	Logo     LogoConfig     `json:"logo"`
	Thermal  ThermalConfig  `json:"thermal"`
	Display  DisplayConfig  `json:"display"`
}

// ── Stats types ───────────────────────────────────────────────────────────────

type TempReading struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Value       int    `json:"value"`
}

type DiskInfo struct {
	Mount   string  `json:"mount"`
	Total   uint64  `json:"total_gb"`
	Used    uint64  `json:"used_gb"`
	Free    uint64  `json:"free_gb"`
	Percent float64 `json:"percent"`
}

type GPUInfo struct {
	Label   string  `json:"label"`
	Vendor  string  `json:"vendor"`
	Temp    int     `json:"temp"`
	Usage   float64 `json:"usage_percent"`
	VRAMUsed  uint64 `json:"vram_used_mb"`
	VRAMTotal uint64 `json:"vram_total_mb"`
}

type StatsResponse struct {
	CPUPercent  float64       `json:"cpu_percent"`
	RAMPercent  float64       `json:"ram_percent"`
	RAMUsedMB   uint64        `json:"ram_used_mb"`
	RAMTotalMB  uint64        `json:"ram_total_mb"`
	Temps       []TempReading `json:"temps"`
	Disks       []DiskInfo    `json:"disks"`
	GPUs        []GPUInfo     `json:"gpus"`
	Load1m      float64       `json:"load_1m"`
	Load5m      float64       `json:"load_5m"`
	Load15m     float64       `json:"load_15m"`
	UptimeSeconds int64       `json:"uptime_seconds"`
	Arch        string        `json:"arch"`
	Timestamp   int64         `json:"timestamp"`
}

type StatusResponse struct {
	Timestamp int64             `json:"timestamp"`
	Services  map[string]string `json:"services"`
}

// ── Paths ─────────────────────────────────────────────────────────────────────

var (
	hostProc   = envOr("HOST_PROC", "/host/proc")
	hostSys    = envOr("HOST_SYS", "/host/sys")
	configDir  = envOr("CONFIG_DIR", "/config")
	listenPort = envOr("PORT", "8080")
	statsInterval  = envIntOr("STATS_INTERVAL", 5)
	statusInterval = envIntOr("STATUS_INTERVAL", 30)
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// ── CPU sampling ──────────────────────────────────────────────────────────────

type cpuSample struct {
	total, idle uint64
}

func readCPUSample() (cpuSample, error) {
	data, err := os.ReadFile(filepath.Join(hostProc, "stat"))
	if err != nil {
		return cpuSample{}, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		vals := make([]uint64, len(fields)-1)
		for i, f := range fields[1:] {
			vals[i], _ = strconv.ParseUint(f, 10, 64)
		}
		// user, nice, system, idle, iowait, irq, softirq
		idle := vals[3] + vals[4]
		total := vals[0] + vals[1] + vals[2] + vals[3] + vals[4] + vals[5] + vals[6]
		return cpuSample{total: total, idle: idle}, nil
	}
	return cpuSample{}, fmt.Errorf("cpu line not found in /proc/stat")
}

// ── RAM ───────────────────────────────────────────────────────────────────────

func readRAM() (usedMB, totalMB uint64, percent float64, err error) {
	data, err := os.ReadFile(filepath.Join(hostProc, "meminfo"))
	if err != nil {
		return
	}
	var total, free, buffers, cached, sreclaimable uint64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val, _ := strconv.ParseUint(fields[1], 10, 64)
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			total = val
		case "MemFree":
			free = val
		case "Buffers":
			buffers = val
		case "Cached":
			cached = val
		case "SReclaimable":
			sreclaimable = val
		}
	}
	used := total - free - buffers - cached - sreclaimable
	totalMB = total / 1024
	usedMB = used / 1024
	if total > 0 {
		percent = math.Round(float64(used)*100/float64(total)*10) / 10
	}
	return
}

// ── Load average ──────────────────────────────────────────────────────────────

func readLoadAvg() (load1, load5, load15 float64, err error) {
	data, err := os.ReadFile(filepath.Join(hostProc, "loadavg"))
	if err != nil {
		return
	}
	fields := strings.Fields(string(data))
	if len(fields) >= 3 {
		load1, _ = strconv.ParseFloat(fields[0], 64)
		load5, _ = strconv.ParseFloat(fields[1], 64)
		load15, _ = strconv.ParseFloat(fields[2], 64)
	}
	return
}

// ── Uptime ────────────────────────────────────────────────────────────────────

func readUptime() (int64, error) {
	data, err := os.ReadFile(filepath.Join(hostProc, "uptime"))
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty uptime")
	}
	f, err := strconv.ParseFloat(fields[0], 64)
	return int64(f), err
}

// ── Temperatures ──────────────────────────────────────────────────────────────

func readThermalZones(zones []ThermalZone) []TempReading {
	// If config provides zones — use them
	if len(zones) > 0 {
		var result []TempReading
		for _, z := range zones {
			// Rewrite path to use hostSys prefix
			path := z.Path
			if strings.HasPrefix(path, "/sys/") {
				path = filepath.Join(hostSys, strings.TrimPrefix(path, "/sys"))
			}
			val := readTempFile(path)
			result = append(result, TempReading{
				Label:       z.Label,
				Description: z.Description,
				Value:       val,
			})
		}
		return result
	}

	// Auto-discover thermal zones
	var result []TempReading
	pattern := filepath.Join(hostSys, "class/thermal/thermal_zone*")
	zones_paths, _ := filepath.Glob(pattern)
	// Track unique types to avoid duplicates
	seen := map[string]bool{}
	for _, zonePath := range zones_paths {
		typeData, err := os.ReadFile(filepath.Join(zonePath, "type"))
		if err != nil {
			continue
		}
		zoneType := strings.TrimSpace(string(typeData))

		// Skip noisy/unreliable zone types
		switch zoneType {
		case "acpitz", "ACPI Air", "pch_skylake", "pch_cannonlake",
			"pch_cometlake", "pch_tigerlake", "pch_alderlake",
			"INT3400 Thermal", "B0D4", "TSR0", "TSR1", "TSR2":
			continue
		}

		// Skip duplicate types — show each type once
		if seen[zoneType] {
			continue
		}
		seen[zoneType] = true

		temp := readTempFile(filepath.Join(zonePath, "temp"))
		// Skip zones reporting 0 or implausible values
		if temp <= 0 || temp > 120 {
			continue
		}

		label := formatZoneLabel(zoneType)
		result = append(result, TempReading{
			Label:       label,
			Description: zoneType,
			Value:       temp,
		})
	}
	return result
}

func formatZoneLabel(zoneType string) string {
	// Make zone types human-readable
	replacer := strings.NewReplacer(
		"cluster0_thermal", "Cluster 0",
		"cluster1_thermal", "Cluster 1",
		"cluster2_thermal", "Cluster 2",
		"cluster3_thermal", "Cluster 3",
		"x86_pkg_temp", "CPU Package",
		"coretemp", "CPU Core",
		"k10temp", "CPU",
		"amdgpu", "AMD GPU",
		"iwlwifi", "WiFi",
		"pch_skylake", "PCH",
		"pch_cannonlake", "PCH",
		"_thermal", "",
	)
	label := replacer.Replace(zoneType)
	if label == zoneType {
		// Fallback: title case
		label = strings.Title(strings.ReplaceAll(zoneType, "_", " "))
	}
	return label
}

func readTempFile(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	val, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return int(val / 1000)
}

// ── GPU ───────────────────────────────────────────────────────────────────────

func readGPUs() []GPUInfo {
	var gpus []GPUInfo

	// Scan DRM cards
	cardPattern := filepath.Join(hostSys, "class/drm/card*")
	cards, _ := filepath.Glob(cardPattern)

	for _, card := range cards {
		// Skip card render nodes
		base := filepath.Base(card)
		if strings.Contains(base, "-") {
			continue
		}

		devicePath := filepath.Join(card, "device")

		// Detect vendor
		vendorData, err := os.ReadFile(filepath.Join(devicePath, "vendor"))
		if err != nil {
			continue
		}
		vendor := strings.TrimSpace(string(vendorData))

		var gpu GPUInfo
		gpu.Label = base

		switch vendor {
		case "0x1002": // AMD
			gpu.Vendor = "AMD"
			gpu.Temp = readGPUTempHwmon(devicePath)
			gpu.Usage = readAMDGPUUsage(devicePath)
			gpu.VRAMUsed, gpu.VRAMTotal = readAMDVRAM(devicePath)

		case "0x8086": // Intel
			gpu.Vendor = "Intel"
			gpu.Temp = readGPUTempHwmon(devicePath)
			gpu.Usage = readIntelGPUUsage(card)

		default:
			continue
		}

		// Only include if we got meaningful data
		if gpu.Temp > 0 || gpu.Usage > 0 {
			gpus = append(gpus, gpu)
		}
	}

	return gpus
}

func readGPUTempHwmon(devicePath string) int {
	// /sys/class/drm/card*/device/hwmon/hwmon*/temp1_input
	hwmonPattern := filepath.Join(devicePath, "hwmon", "hwmon*", "temp1_input")
	matches, _ := filepath.Glob(hwmonPattern)
	for _, m := range matches {
		if t := readTempFile(m); t > 0 {
			return t
		}
	}
	return 0
}

func readAMDGPUUsage(devicePath string) float64 {
	data, err := os.ReadFile(filepath.Join(devicePath, "gpu_busy_percent"))
	if err != nil {
		return 0
	}
	val, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	if err != nil {
		return 0
	}
	return val
}

func readAMDVRAM(devicePath string) (usedMB, totalMB uint64) {
	readMB := func(file string) uint64 {
		data, err := os.ReadFile(filepath.Join(devicePath, file))
		if err != nil {
			return 0
		}
		val, _ := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		return val / (1024 * 1024)
	}
	totalMB = readMB("mem_info_vram_total")
	usedMB = readMB("mem_info_vram_used")
	return
}

func readIntelGPUUsage(cardPath string) float64 {
	// Intel GPU usage via rc6 residency — rough approximation
	// rc6_residency_ms gives time GPU was idle — invert for usage
	data, err := os.ReadFile(filepath.Join(cardPath, "gt", "gt0", "rc6_residency_ms"))
	if err != nil {
		return 0
	}
	_ = data
	// rc6 residency is complex to calculate accurately without baseline
	// Return 0 for now — temp is more useful than a misleading usage %
	return 0
}

// ── Disk ──────────────────────────────────────────────────────────────────────

func readDisks() []DiskInfo {
	data, err := os.ReadFile(filepath.Join(hostProc, "mounts"))
	if err != nil {
		return nil
	}

	seen := map[string]bool{}
	var disks []DiskInfo

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		device := fields[0]
		mount := fields[1]
		fstype := fields[2]

		// Only real filesystems
		if !strings.HasPrefix(device, "/dev/") {
			continue
		}
		// Skip docker overlays, tmpfs, etc
		switch fstype {
		case "overlay", "tmpfs", "devtmpfs", "squashfs":
			continue
		}
		// Skip duplicate mounts of same device
		if seen[device] {
			continue
		}
		seen[device] = true

		var stat syscall.Statfs_t
		if err := syscall.Statfs(mount, &stat); err != nil {
			continue
		}

		total := stat.Blocks * uint64(stat.Bsize)
		free := stat.Bavail * uint64(stat.Bsize)
		used := total - free

		if total == 0 {
			continue
		}

		disks = append(disks, DiskInfo{
			Mount:   mount,
			Total:   total / (1024 * 1024 * 1024),
			Used:    used / (1024 * 1024 * 1024),
			Free:    free / (1024 * 1024 * 1024),
			Percent: math.Round(float64(used)*100/float64(total)*10) / 10,
		})
	}
	return disks
}

// ── Arch detection ────────────────────────────────────────────────────────────

func detectArch() string {
	data, err := os.ReadFile(filepath.Join(hostProc, "cpuinfo"))
	if err != nil {
		return ""
	}
	content := string(data)

	// RISC-V
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "isa") {
			isa := strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
			summary := "rv64"
			if strings.Contains(isa, "imafd") || strings.Contains(isa, "g") {
				summary += "g"
			}
			if strings.Contains(isa, "c") {
				summary += "c"
			}
			if strings.Contains(isa, "v") {
				summary += "v"
			}
			arch := "RISC-V " + summary
			// Add uarch
			for _, l := range strings.Split(content, "\n") {
				if strings.HasPrefix(l, "uarch") {
					parts := strings.Split(strings.TrimSpace(strings.SplitN(l, ":", 2)[1]), ",")
					if len(parts) > 1 {
						arch += " · " + strings.ToUpper(parts[1])
					}
					break
				}
			}
			return arch
		}
	}

	// ARM
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "CPU architecture") {
			v := strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
			if v == "8" {
				return "ARM64"
			}
			return "ARMv" + v
		}
	}

	// x86
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "flags") {
			if strings.Contains(line, " lm ") || strings.Contains(line, " lm\t") {
				return "x86-64"
			}
			return "x86"
		}
		if strings.HasPrefix(line, "model name") {
			if strings.Contains(strings.ToLower(line), "intel") || strings.Contains(strings.ToLower(line), "amd") {
				return "x86-64"
			}
		}
	}

	return ""
}


// ── Hardware auto-detection ───────────────────────────────────────────────────

func detectCPU() string {
	data, err := os.ReadFile(filepath.Join(hostProc, "cpuinfo"))
	if err != nil {
		return ""
	}
	var modelName, hardware string
	cores := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "model name") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 && modelName == "" {
				modelName = strings.TrimSpace(parts[1])
			}
		}
		if strings.HasPrefix(line, "Hardware") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				hardware = strings.TrimSpace(parts[1])
			}
		}
		if strings.HasPrefix(line, "processor") {
			cores++
		}
	}
	name := modelName
	if name == "" {
		name = hardware
	}
	if name == "" {
		return ""
	}
	if cores > 0 {
		return fmt.Sprintf("%s · %d cores", name, cores)
	}
	return name
}

func detectOS() string {
	// Try host os-release via /host/proc/1/root
	// /host/proc/1/root symlinks to the host root filesystem
	for _, path := range []string{
		"/host/proc/1/root/etc/os-release",
		"/etc/os-release",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), "\"")
			}
		}
	}
	return ""
}

func detectKernel() string {
	data, err := os.ReadFile(filepath.Join(hostProc, "version"))
	if err != nil {
		return ""
	}
	// Format: "Linux version 6.x.x ..."
	fields := strings.Fields(string(data))
	if len(fields) >= 3 {
		return fields[2]
	}
	return ""
}

func detectRAM() string {
	data, err := os.ReadFile(filepath.Join(hostProc, "meminfo"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseUint(fields[1], 10, 64)
				if err == nil {
					gb := float64(kb) / (1024 * 1024)
					if gb >= 1 {
						return fmt.Sprintf("%.0f GB", gb)
					}
					return fmt.Sprintf("%.1f GB", gb)
				}
			}
		}
	}
	return ""
}

func detectBoard() string {
	// Try DMI (x86) — hostSys is /host/sys, dmi is under class/dmi/id/
	for _, name := range []string{"product_name", "board_name"} {
		path := filepath.Join(hostSys, "class", "dmi", "id", name)
		data, err := os.ReadFile(path)
		if err == nil {
			val := strings.TrimSpace(string(data))
			if val != "" &&
				val != "To be filled by O.E.M." &&
				val != "Default string" &&
				val != "None" {
				return val
			}
		}
	}
	// Try device-tree model (ARM/RISC-V)
	data, err := os.ReadFile(filepath.Join(hostProc, "device-tree", "model"))
	if err == nil {
		name := strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", ""))
		if name != "" {
			return name
		}
	}
	// Try cpuinfo Hardware field (older ARM)
	cpudata, err := os.ReadFile(filepath.Join(hostProc, "cpuinfo"))
	if err == nil {
		for _, line := range strings.Split(string(cpudata), "\n") {
			if strings.HasPrefix(line, "Hardware") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					return strings.TrimSpace(parts[1])
				}
			}
		}
	}
	return ""
}

func detectStorage() string {
	data, err := os.ReadFile(filepath.Join(hostProc, "mounts"))
	if err != nil {
		return ""
	}
	var totalGB uint64
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		device := fields[0]
		mount := fields[1]
		fstype := fields[2]
		if !strings.HasPrefix(device, "/dev/") {
			continue
		}
		switch fstype {
		case "overlay", "tmpfs", "devtmpfs", "squashfs", "ramfs":
			continue
		}
		if strings.HasPrefix(mount, "/proc") || strings.HasPrefix(mount, "/sys") ||
			strings.HasPrefix(mount, "/dev") || strings.HasPrefix(mount, "/run") ||
			strings.HasPrefix(mount, "/host") {
			continue
		}
		info, err := os.Stat(mount)
		if err != nil || !info.IsDir() {
			continue
		}
		if seen[device] {
			continue
		}
		seen[device] = true
		var stat syscall.Statfs_t
		if err := syscall.Statfs(mount, &stat); err == nil {
			totalGB += stat.Blocks * uint64(stat.Bsize) / (1024 * 1024 * 1024)
		}
	}
	if totalGB > 0 {
		return fmt.Sprintf("%d GB", totalGB)
	}
	return ""
}

// mergeHardwareConfig auto-detects all hardware fields.
// Config values override auto-detected values only if non-empty.
// JSON null and "" both result in auto-detection.
func mergeHardwareConfig(h *HardwareConfig) {
	// Always auto-detect first
	autoBoard   := detectBoard()
	autoCPU     := detectCPU()
	autoRAM     := detectRAM()
	autoStorage := detectStorage()
	autoOS      := detectOS()
	autoKernel  := detectKernel()

	// Use auto-detected value unless config provides a non-empty override
	if strings.TrimSpace(h.Board) == "" {
		h.Board = autoBoard
	}
	if strings.TrimSpace(h.CPU) == "" {
		h.CPU = autoCPU
	}
	if strings.TrimSpace(h.RAM) == "" {
		h.RAM = autoRAM
	}
	if strings.TrimSpace(h.Storage) == "" {
		h.Storage = autoStorage
	}
	if strings.TrimSpace(h.OS) == "" {
		h.OS = autoOS
	}
	if strings.TrimSpace(h.Kernel) == "" {
		h.Kernel = autoKernel
	}
}

// ── Stats collector ───────────────────────────────────────────────────────────

type StatsCollector struct {
	mu       sync.RWMutex
	current  StatsResponse
	prevSample cpuSample
	arch     string
	config   *DashboardConfig
}

func NewStatsCollector(cfg *DashboardConfig) *StatsCollector {
	s := &StatsCollector{config: cfg}
	s.arch = detectArch()
	if cfg != nil && cfg.Hardware.Arch != "" {
		s.arch = cfg.Hardware.Arch
	}
	// Prime CPU sample
	s.prevSample, _ = readCPUSample()
	return s
}

func (s *StatsCollector) Collect() {
	// CPU — delta from previous sample
	curr, err := readCPUSample()
	var cpuPct float64
	if err == nil {
		diffTotal := curr.total - s.prevSample.total
		diffIdle := curr.idle - s.prevSample.idle
		if diffTotal > 0 {
			cpuPct = math.Round(float64(diffTotal-diffIdle)*100/float64(diffTotal)*10) / 10
		}
		s.prevSample = curr
	}

	ramUsed, ramTotal, ramPct, _ := readRAM()
	load1, load5, load15, _ := readLoadAvg()
	uptime, _ := readUptime()

	var zones []ThermalZone
	if s.config != nil {
		zones = s.config.Thermal.Zones
	}
	temps := readThermalZones(zones)
	disks := readDisks()
	gpus := readGPUs()

	s.mu.Lock()
	s.current = StatsResponse{
		CPUPercent:    cpuPct,
		RAMPercent:    ramPct,
		RAMUsedMB:     ramUsed,
		RAMTotalMB:    ramTotal,
		Temps:         temps,
		Disks:         disks,
		GPUs:          gpus,
		Load1m:        load1,
		Load5m:        load5,
		Load15m:       load15,
		UptimeSeconds: uptime,
		Arch:          s.arch,
		Timestamp:     time.Now().Unix(),
	}
	s.mu.Unlock()
}

func (s *StatsCollector) Get() StatsResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

func (s *StatsCollector) Run(interval time.Duration) {
	s.Collect() // immediate first collection
	ticker := time.NewTicker(interval)
	for range ticker.C {
		s.Collect()
	}
}

// ── Status checker ────────────────────────────────────────────────────────────

type ServicesConfig struct {
	Servers []struct {
		Services []struct {
			URL      string `json:"url"`
			Disabled bool   `json:"disabled"`
		} `json:"services"`
	} `json:"servers"`
}

type StatusChecker struct {
	mu       sync.RWMutex
	current  StatusResponse
	client   *http.Client
	configDir string
}

func NewStatusChecker(configDir string) *StatusChecker {
	return &StatusChecker{
		configDir: configDir,
		client: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: tlsConfigInsecure(),
			},
		},
	}
}

func (sc *StatusChecker) loadURLs() []string {
	data, err := os.ReadFile(filepath.Join(sc.configDir, "services.json"))
	if err != nil {
		return nil
	}
	var cfg ServicesConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	var urls []string
	for _, server := range cfg.Servers {
		for _, svc := range server.Services {
			if !svc.Disabled && svc.URL != "" {
				urls = append(urls, svc.URL)
			}
		}
	}
	return urls
}

func (sc *StatusChecker) Check() {
	urls := sc.loadURLs()
	results := make(map[string]string, len(urls))

	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, url := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			state := "offline"
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
			if err == nil {
				resp, err := sc.client.Do(req)
				if err == nil {
					resp.Body.Close()
					// Any HTTP response = service is running
					state = "online"
				}
			}
			mu.Lock()
			results[u] = state
			mu.Unlock()
		}(url)
	}
	wg.Wait()

	sc.mu.Lock()
	sc.current = StatusResponse{
		Timestamp: time.Now().Unix(),
		Services:  results,
	}
	sc.mu.Unlock()
}

func (sc *StatusChecker) Get() StatusResponse {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return sc.current
}

func (sc *StatusChecker) Run(interval time.Duration) {
	sc.Check()
	ticker := time.NewTicker(interval)
	for range ticker.C {
		sc.Check()
	}
}

// ── TLS helper ────────────────────────────────────────────────────────────────

func tlsConfigInsecure() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true}
}

// ── HTTP handlers ─────────────────────────────────────────────────────────────

func jsonHandler(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	log.SetFlags(log.Ltime | log.Lmsgprefix)
	log.SetPrefix("hs-dashboard ")
	log.Printf("starting on port %s", listenPort)
	log.Printf("host proc: %s", hostProc)
	log.Printf("host sys:  %s", hostSys)
	log.Printf("config:    %s", configDir)

	// Load dashboard config
	var cfg *DashboardConfig
	cfgData, err := os.ReadFile(filepath.Join(configDir, "config.json"))
	if err != nil {
		log.Printf("warning: no config.json found — auto-detecting hardware")
		cfg = &DashboardConfig{}
		cfg.Server.Name = "server"
		cfg.Server.FQDN = "localhost"
		cfg.Display.Theme = "auto"
		cfg.Display.Scale = 1.0
		cfg.Display.Timezone = "UTC"
		cfg.Display.RefreshStatsMs = 5000
		cfg.Display.RefreshStatusMs = 30000
		mergeHardwareConfig(&cfg.Hardware)
	} else {
		cfg = &DashboardConfig{}
		if err := json.Unmarshal(cfgData, cfg); err != nil {
			log.Printf("warning: config.json parse error: %v — using defaults", err)
			cfg = &DashboardConfig{}
			mergeHardwareConfig(&cfg.Hardware)
		} else {
			log.Printf("config loaded: %s (%s)", cfg.Server.Name, cfg.Server.FQDN)
			mergeHardwareConfig(&cfg.Hardware)
		}
	}

	// Start collectors
	stats := NewStatsCollector(cfg)
	go stats.Run(time.Duration(statsInterval) * time.Second)

	status := NewStatusChecker(configDir)
	go status.Run(time.Duration(statusInterval) * time.Second)

	// Static files from embedded www/
	wwwFS, err := fs.Sub(staticFiles, "www")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()

	// API endpoints
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		jsonHandler(w, stats.Get())
	})

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		jsonHandler(w, status.Get())
	})

	// Serve config files from mounted /config directory
	mux.HandleFunc("/config.json", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(configDir, "config.json"))
	})

	mux.HandleFunc("/services.json", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(configDir, "services.json"))
	})

	// Static files (embedded)
	mux.Handle("/", http.FileServer(http.FS(wwwFS)))

	log.Printf("ready — http://0.0.0.0:%s", listenPort)
	if err := http.ListenAndServe(":"+listenPort, mux); err != nil {
		log.Fatal(err)
	}
}
