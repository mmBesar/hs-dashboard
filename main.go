// hs-dashboard — main.go
// Single binary dashboard server.
// Reads host metrics from /host/proc and /host/sys (mounted read-only).
// Serves static files embedded at compile time.
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

//go:embed www
var staticFiles embed.FS

// ── Config types ──────────────────────────────────────────────────────────────

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
	Arch    string `json:"arch"`
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
	Timezone        string  `json:"timezone"`
	Theme           string  `json:"theme"`
	Scale           float64 `json:"scale"`
	RefreshStatsMs  int     `json:"refresh_stats_ms"`
	RefreshStatusMs int     `json:"refresh_status_ms"`
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
	TotalGB uint64  `json:"total_gb"`
	UsedGB  uint64  `json:"used_gb"`
	FreeGB  uint64  `json:"free_gb"`
	Percent float64 `json:"percent"`
}

type GPUInfo struct {
	Label     string  `json:"label"`
	Vendor    string  `json:"vendor"`
	Temp      int     `json:"temp"`
	Usage     float64 `json:"usage_percent"`
	VRAMUsed  uint64  `json:"vram_used_mb"`
	VRAMTotal uint64  `json:"vram_total_mb"`
}

type StatsResponse struct {
	CPUPercent    float64       `json:"cpu_percent"`
	RAMPercent    float64       `json:"ram_percent"`
	RAMUsedMB     uint64        `json:"ram_used_mb"`
	RAMTotalMB    uint64        `json:"ram_total_mb"`
	Temps         []TempReading `json:"temps"`
	Disks         []DiskInfo    `json:"disks"`
	GPUs          []GPUInfo     `json:"gpus"`
	Load1m        float64       `json:"load_1m"`
	Load5m        float64       `json:"load_5m"`
	Load15m       float64       `json:"load_15m"`
	UptimeSeconds int64         `json:"uptime_seconds"`
	Arch          string        `json:"arch"`
	Timestamp     int64         `json:"timestamp"`
}

type StatusResponse struct {
	Timestamp int64             `json:"timestamp"`
	Services  map[string]string `json:"services"`
}

// ── Env helpers ───────────────────────────────────────────────────────────────

var (
	hostProc        = envOr("HOST_PROC", "/host/proc")
	hostSys         = envOr("HOST_SYS", "/host/sys")
	configDir       = envOr("CONFIG_DIR", "/config")
	listenPort      = envOr("PORT", "8080")
	statsInterval   = envIntOr("STATS_INTERVAL", 5)
	statusInterval  = envIntOr("STATUS_INTERVAL", 30)
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

// ── CPU ───────────────────────────────────────────────────────────────────────

type cpuSample struct{ total, idle uint64 }

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
		idle := vals[3] + vals[4]
		total := vals[0] + vals[1] + vals[2] + vals[3] + vals[4] + vals[5] + vals[6]
		return cpuSample{total: total, idle: idle}, nil
	}
	return cpuSample{}, fmt.Errorf("cpu line not found")
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

// ── Load + Uptime ─────────────────────────────────────────────────────────────

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

func formatZoneLabel(zoneType string) string {
	labels := map[string]string{
		"cluster0_thermal": "Cluster 0",
		"cluster1_thermal": "Cluster 1",
		"cluster2_thermal": "Cluster 2",
		"cluster3_thermal": "Cluster 3",
		"x86_pkg_temp":     "CPU Package",
		"coretemp":         "CPU Core",
		"k10temp":          "CPU",
		"amdgpu":           "AMD GPU",
		"iwlwifi":          "WiFi",
	}
	if l, ok := labels[zoneType]; ok {
		return l
	}
	s := strings.ReplaceAll(zoneType, "_thermal", "")
	s = strings.ReplaceAll(s, "_", " ")
	return strings.Title(s)
}

func readThermalZones(zones []ThermalZone) []TempReading {
	if len(zones) > 0 {
		var result []TempReading
		for _, z := range zones {
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
	// Auto-discover
	var result []TempReading
	seen := map[string]bool{}
	pattern := filepath.Join(hostSys, "class/thermal/thermal_zone*")
	zonePaths, _ := filepath.Glob(pattern)
	for _, zonePath := range zonePaths {
		typeData, err := os.ReadFile(filepath.Join(zonePath, "type"))
		if err != nil {
			continue
		}
		zoneType := strings.TrimSpace(string(typeData))
		switch zoneType {
		case "acpitz", "ACPI Air", "pch_skylake", "pch_cannonlake",
			"pch_cometlake", "pch_tigerlake", "pch_alderlake",
			"INT3400 Thermal", "B0D4", "TSR0", "TSR1", "TSR2":
			continue
		}
		if seen[zoneType] {
			continue
		}
		seen[zoneType] = true
		temp := readTempFile(filepath.Join(zonePath, "temp"))
		if temp <= 0 || temp > 120 {
			continue
		}
		result = append(result, TempReading{
			Label:       formatZoneLabel(zoneType),
			Description: zoneType,
			Value:       temp,
		})
	}
	return result
}

// ── GPU ───────────────────────────────────────────────────────────────────────

func readGPUTempHwmon(devicePath string) int {
	pattern := filepath.Join(devicePath, "hwmon", "hwmon*", "temp1_input")
	matches, _ := filepath.Glob(pattern)
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
	val, _ := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
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

func readGPUs() []GPUInfo {
	var gpus []GPUInfo
	cards, _ := filepath.Glob(filepath.Join(hostSys, "class/drm/card*"))
	for _, card := range cards {
		if strings.Contains(filepath.Base(card), "-") {
			continue
		}
		devicePath := filepath.Join(card, "device")
		vendorData, err := os.ReadFile(filepath.Join(devicePath, "vendor"))
		if err != nil {
			continue
		}
		vendor := strings.TrimSpace(string(vendorData))
		var gpu GPUInfo
		gpu.Label = filepath.Base(card)
		switch vendor {
		case "0x1002":
			gpu.Vendor = "AMD"
			gpu.Temp = readGPUTempHwmon(devicePath)
			gpu.Usage = readAMDGPUUsage(devicePath)
			gpu.VRAMUsed, gpu.VRAMTotal = readAMDVRAM(devicePath)
		case "0x8086":
			gpu.Vendor = "Intel"
			gpu.Temp = readGPUTempHwmon(devicePath)
		default:
			continue
		}
		if gpu.Temp > 0 || gpu.Usage > 0 {
			gpus = append(gpus, gpu)
		}
	}
	return gpus
}

// ── Disks ─────────────────────────────────────────────────────────────────────

// isRealDisk — strict filter for display: only / and single-level mounts
func isRealDisk(device, mount, fstype string) bool {
	if !strings.HasPrefix(device, "/dev/") {
		return false
	}
	switch fstype {
	case "overlay", "tmpfs", "devtmpfs", "squashfs", "ramfs",
		"sysfs", "proc", "devpts", "cgroup", "cgroup2",
		"pstore", "bpf", "tracefs", "debugfs", "securityfs",
		"fusectl", "hugetlbfs", "mqueue", "autofs", "rpc_pipefs":
		return false
	}
	for _, prefix := range []string{"/proc", "/sys", "/dev", "/run", "/host", "/snap"} {
		if strings.HasPrefix(mount, prefix) {
			return false
		}
	}
	// Only / and single-level mounts (e.g. /data, /home) — excludes file bind mounts
	parts := strings.Split(strings.Trim(mount, "/"), "/")
	if len(parts) > 1 {
		return false
	}
	if strings.Contains(filepath.Base(mount), ".") {
		return false
	}
	info, err := os.Stat(mount)
	if err != nil || !info.IsDir() {
		return false
	}
	return true
}

// isRealDevice — less strict, for storage total calculation
func isRealDevice(device, fstype string) bool {
	if !strings.HasPrefix(device, "/dev/") {
		return false
	}
	switch fstype {
	case "overlay", "tmpfs", "devtmpfs", "squashfs", "ramfs",
		"sysfs", "proc", "devpts", "cgroup", "cgroup2",
		"pstore", "bpf", "tracefs", "debugfs", "securityfs",
		"fusectl", "hugetlbfs", "mqueue", "autofs", "rpc_pipefs":
		return false
	}
	return true
}

func readMounts() []string {
	data, err := os.ReadFile(filepath.Join(hostProc, "mounts"))
	if err != nil {
		return nil
	}
	return strings.Split(string(data), "\n")
}

func readDisks() []DiskInfo {
	seen := map[string]bool{}
	var disks []DiskInfo
	for _, line := range readMounts() {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		device, mount, fstype := fields[0], fields[1], fields[2]
		if !isRealDisk(device, mount, fstype) {
			continue
		}
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
		if total == 0 || total < 256*1024*1024 {
			continue
		}
		label := filepath.Base(device)
		if mount == "/" {
			label = filepath.Base(device) + " (/)"
		}
		disks = append(disks, DiskInfo{
			Mount:   label,
			TotalGB: total / (1024 * 1024 * 1024),
			UsedGB:  used / (1024 * 1024 * 1024),
			FreeGB:  free / (1024 * 1024 * 1024),
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
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "isa") {
			isa := strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
			summary := "rv64"
			if strings.Contains(isa, "imafd") {
				summary += "g"
			}
			if strings.Contains(isa, "c") {
				summary += "c"
			}
			if strings.Contains(isa, "v") {
				summary += "v"
			}
			arch := "RISC-V " + summary
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
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "CPU architecture") {
			v := strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
			if v == "8" {
				return "ARM64"
			}
			return "ARMv" + v
		}
	}
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "flags") {
			if strings.Contains(line, " lm") {
				return "x86-64"
			}
			return "x86"
		}
	}
	return ""
}

// ── Hardware auto-detection ───────────────────────────────────────────────────

func isValidBoardName(name string) bool {
	if name == "" {
		return false
	}
	invalid := []string{
		"To be filled by O.E.M.", "Default string", "None",
		"Not Applicable", "N/A", "System Product Name",
		"System Version", "Base Board Product Name",
	}
	for _, inv := range invalid {
		if strings.EqualFold(name, inv) {
			return false
		}
	}
	if strings.Contains(name, "[") && strings.Contains(name, "]") {
		return false
	}
	return true
}

func detectBoard() string {
	for _, name := range []string{"product_name", "board_name"} {
		path := filepath.Join(hostSys, "class", "dmi", "id", name)
		data, err := os.ReadFile(path)
		if err == nil {
			if val := strings.TrimSpace(string(data)); isValidBoardName(val) {
				return val
			}
		}
	}
	for _, dtPath := range []string{
		filepath.Join(hostProc, "device-tree", "model"),
		filepath.Join(hostSys, "firmware", "devicetree", "base", "model"),
	} {
		data, err := os.ReadFile(dtPath)
		if err == nil {
			name := strings.TrimSpace(string(data))
			name = strings.ReplaceAll(name, "\x00", "")
			if name != "" {
				return name
			}
		}
	}
	cpudata, _ := os.ReadFile(filepath.Join(hostProc, "cpuinfo"))
	for _, line := range strings.Split(string(cpudata), "\n") {
		if strings.HasPrefix(line, "Hardware") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

func detectCPU() string {
	data, _ := os.ReadFile(filepath.Join(hostProc, "cpuinfo"))
	var modelName, hardware string
	cores := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "model name") && modelName == "" {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
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

func detectRAM() string {
	data, _ := os.ReadFile(filepath.Join(hostProc, "meminfo"))
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

func detectOS() string {
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
	fields := strings.Fields(string(data))
	if len(fields) >= 3 {
		return fields[2]
	}
	return ""
}

func detectStorage() string {
	seen := map[string]bool{}
	var totalGB uint64
	for _, line := range readMounts() {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		device, mount, fstype := fields[0], fields[1], fields[2]
		if !isRealDevice(device, fstype) {
			continue
		}
		skip := false
		for _, prefix := range []string{"/proc", "/sys", "/dev", "/run", "/host", "/snap"} {
			if strings.HasPrefix(mount, prefix) {
				skip = true
				break
			}
		}
		if skip || seen[device] {
			continue
		}
		seen[device] = true
		var stat syscall.Statfs_t
		if err := syscall.Statfs(mount, &stat); err == nil {
			gb := stat.Blocks * uint64(stat.Bsize) / (1024 * 1024 * 1024)
			if gb > 0 {
				totalGB += gb
			}
		}
	}
	if totalGB == 0 {
		return ""
	}
	if totalGB >= 1024 {
		return fmt.Sprintf("%.1f TB", float64(totalGB)/1024)
	}
	return fmt.Sprintf("%d GB", totalGB)
}

func mergeHardwareConfig(h *HardwareConfig) {
	autoBoard := detectBoard()
	autoCPU := detectCPU()
	autoRAM := detectRAM()
	autoStorage := detectStorage()
	autoOS := detectOS()
	autoKernel := detectKernel()

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

	log.Printf("hardware: board=%q cpu=%q ram=%q storage=%q os=%q kernel=%q",
		h.Board, h.CPU, h.RAM, h.Storage, h.OS, h.Kernel)
}

// ── JSON helpers ──────────────────────────────────────────────────────────────

func stripCommentKeysDeep(data []byte) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err == nil {
		result := make(map[string]json.RawMessage)
		for k, v := range raw {
			if strings.HasPrefix(k, "_") {
				continue
			}
			result[k] = json.RawMessage(stripCommentKeysDeep(v))
		}
		if b, err := json.Marshal(result); err == nil {
			return b
		}
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err == nil {
		result := make([]json.RawMessage, len(arr))
		for i, v := range arr {
			result[i] = json.RawMessage(stripCommentKeysDeep(v))
		}
		if b, err := json.Marshal(result); err == nil {
			return b
		}
	}
	return data
}

// ── Stats collector ───────────────────────────────────────────────────────────

type StatsCollector struct {
	mu         sync.RWMutex
	current    StatsResponse
	prevSample cpuSample
	arch       string
	config     *DashboardConfig
}

func NewStatsCollector(cfg *DashboardConfig) *StatsCollector {
	s := &StatsCollector{config: cfg}
	s.arch = detectArch()
	if cfg != nil && cfg.Hardware.Arch != "" {
		s.arch = cfg.Hardware.Arch
	}
	s.prevSample, _ = readCPUSample()
	return s
}

func (s *StatsCollector) Collect() {
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
	s.Collect()
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
	mu        sync.RWMutex
	current   StatusResponse
	client    *http.Client
	configDir string
}

func NewStatusChecker(dir string) *StatusChecker {
	return &StatusChecker{
		configDir: dir,
		client: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

func (sc *StatusChecker) loadURLs() []string {
	data, err := os.ReadFile(filepath.Join(sc.configDir, "services.json"))
	if err != nil {
		return nil
	}
	cleaned := stripCommentKeysDeep(data)
	var cfg ServicesConfig
	if err := json.Unmarshal(cleaned, &cfg); err != nil {
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
	sc.current = StatusResponse{Timestamp: time.Now().Unix(), Services: results}
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

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func jsonHandler(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(data)
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	log.SetFlags(log.Ltime | log.Lmsgprefix)
	log.SetPrefix("hs-dashboard ")
	log.Printf("starting on port %s", listenPort)
	log.Printf("host proc: %s | host sys: %s | config: %s", hostProc, hostSys, configDir)

	// Load config with defaults
	cfg := &DashboardConfig{}
	cfg.Display.Theme = "auto"
	cfg.Display.Scale = 1.2
	cfg.Display.Timezone = "UTC"
	cfg.Display.RefreshStatsMs = 5000
	cfg.Display.RefreshStatusMs = 30000

	cfgData, err := os.ReadFile(filepath.Join(configDir, "config.json"))
	if err != nil {
		log.Printf("no config.json — using auto-detected values")
	} else {
		cleaned := stripCommentKeysDeep(cfgData)
		if err2 := json.Unmarshal(cleaned, cfg); err2 != nil {
			log.Printf("config.json parse error: %v", err2)
		} else {
			log.Printf("config loaded: %s (%s)", cfg.Server.Name, cfg.Server.FQDN)
		}
	}

	// Auto-detect hardware — config values override if non-empty
	mergeHardwareConfig(&cfg.Hardware)

	// Start collectors
	stats := NewStatsCollector(cfg)
	go stats.Run(time.Duration(statsInterval) * time.Second)

	status := NewStatusChecker(configDir)
	go status.Run(time.Duration(statusInterval) * time.Second)

	// Static files
	wwwFS, err := fs.Sub(staticFiles, "www")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()

	// API
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		jsonHandler(w, stats.Get())
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		jsonHandler(w, status.Get())
	})

	// Serve merged config (auto-detected values fill in nulls)
	mux.HandleFunc("/config.json", func(w http.ResponseWriter, r *http.Request) {
		jsonHandler(w, cfg)
	})

	// Serve services.json with _comment_ keys stripped
	mux.HandleFunc("/services.json", func(w http.ResponseWriter, r *http.Request) {
		data, err := os.ReadFile(filepath.Join(configDir, "services.json"))
		if err != nil {
			http.Error(w, "services.json not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(stripCommentKeysDeep(data))
	})

	// Static files
	mux.Handle("/", http.FileServer(http.FS(wwwFS)))

	log.Printf("ready — http://0.0.0.0:%s", listenPort)
	if err := http.ListenAndServe(":"+listenPort, mux); err != nil {
		log.Fatal(err)
	}
}
