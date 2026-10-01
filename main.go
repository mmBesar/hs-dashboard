// hs-dashboard — main.go
// Single binary homelab dashboard.
// Reads host metrics via /host/proc and /host/sys (read-only mounts).
// Embeds static files at compile time — no external dependencies.
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

// ── Environment ───────────────────────────────────────────────────────────────

var (
	hostProc       = envOr("HOST_PROC", "/host/proc")
	hostSys        = envOr("HOST_SYS", "/host/sys")
	configDir      = envOr("CONFIG_DIR", "/config")
	listenPort     = envOr("PORT", "8080")
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
	TileWidth       int     `json:"tile_width"`
	ShowURLs        bool    `json:"show_urls"`
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

type DiskStat struct {
	Device  string  `json:"device"`
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
	Disks         []DiskStat    `json:"disks"`
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

// ── CPU sampling ──────────────────────────────────────────────────────────────

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
		var vals [7]uint64
		for i := range vals {
			vals[i], _ = strconv.ParseUint(fields[i+1], 10, 64)
		}
		idle := vals[3] + vals[4]
		total := vals[0] + vals[1] + vals[2] + vals[3] + vals[4] + vals[5] + vals[6]
		return cpuSample{total: total, idle: idle}, nil
	}
	return cpuSample{}, fmt.Errorf("cpu line not found")
}

// ── RAM ───────────────────────────────────────────────────────────────────────

func readRAM() (usedMB, totalMB uint64, percent float64) {
	data, err := os.ReadFile(filepath.Join(hostProc, "meminfo"))
	if err != nil {
		return
	}
	var total, free, buffers, cached, sreclaimable uint64
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		switch strings.TrimSuffix(f[0], ":") {
		case "MemTotal":
			total = v
		case "MemFree":
			free = v
		case "Buffers":
			buffers = v
		case "Cached":
			cached = v
		case "SReclaimable":
			sreclaimable = v
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

func readLoadAvg() (l1, l5, l15 float64) {
	data, _ := os.ReadFile(filepath.Join(hostProc, "loadavg"))
	f := strings.Fields(string(data))
	if len(f) >= 3 {
		l1, _ = strconv.ParseFloat(f[0], 64)
		l5, _ = strconv.ParseFloat(f[1], 64)
		l15, _ = strconv.ParseFloat(f[2], 64)
	}
	return
}

func readUptime() int64 {
	data, _ := os.ReadFile(filepath.Join(hostProc, "uptime"))
	f := strings.Fields(string(data))
	if len(f) > 0 {
		v, _ := strconv.ParseFloat(f[0], 64)
		return int64(v)
	}
	return 0
}

// ── Temperatures ──────────────────────────────────────────────────────────────

func readMilliTemp(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return int(v / 1000)
}

var zoneLabels = map[string]string{
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

var skipZones = map[string]bool{
	"acpitz": true, "ACPI Air": true,
	"pch_skylake": true, "pch_cannonlake": true,
	"pch_cometlake": true, "pch_tigerlake": true,
	"pch_alderlake": true, "INT3400 Thermal": true,
	"B0D4": true, "TSR0": true, "TSR1": true, "TSR2": true,
}

func zoneLabel(zoneType string) string {
	if l, ok := zoneLabels[zoneType]; ok {
		return l
	}
	s := strings.ReplaceAll(zoneType, "_thermal", "")
	s = strings.ReplaceAll(s, "_", " ")
	words := strings.Fields(s)
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

func readTemps(zones []ThermalZone) []TempReading {
	// Manual zones from config
	if len(zones) > 0 {
		var out []TempReading
		for _, z := range zones {
			path := z.Path
			if strings.HasPrefix(path, "/sys/") {
				path = filepath.Join(hostSys, strings.TrimPrefix(path, "/sys"))
			}
			out = append(out, TempReading{
				Label:       z.Label,
				Description: z.Description,
				Value:       readMilliTemp(path),
			})
		}
		return out
	}
	// Auto-discover
	var out []TempReading
	seen := map[string]bool{}
	paths, _ := filepath.Glob(filepath.Join(hostSys, "class/thermal/thermal_zone*"))
	for _, p := range paths {
		typeBytes, err := os.ReadFile(filepath.Join(p, "type"))
		if err != nil {
			continue
		}
		t := strings.TrimSpace(string(typeBytes))
		if skipZones[t] || seen[t] {
			continue
		}
		seen[t] = true
		v := readMilliTemp(filepath.Join(p, "temp"))
		if v <= 0 || v > 120 {
			continue
		}
		out = append(out, TempReading{Label: zoneLabel(t), Description: t, Value: v})
	}
	return out
}

// ── GPU ───────────────────────────────────────────────────────────────────────

func gpuTempHwmon(devPath string) int {
	matches, _ := filepath.Glob(filepath.Join(devPath, "hwmon", "hwmon*", "temp1_input"))
	for _, m := range matches {
		if v := readMilliTemp(m); v > 0 {
			return v
		}
	}
	return 0
}

func readGPUs() []GPUInfo {
	var out []GPUInfo
	cards, _ := filepath.Glob(filepath.Join(hostSys, "class/drm/card*"))
	for _, card := range cards {
		if strings.Contains(filepath.Base(card), "-") {
			continue
		}
		devPath := filepath.Join(card, "device")
		vb, err := os.ReadFile(filepath.Join(devPath, "vendor"))
		if err != nil {
			continue
		}
		vendor := strings.TrimSpace(string(vb))
		g := GPUInfo{Label: filepath.Base(card)}
		switch vendor {
		case "0x1002": // AMD
			g.Vendor = "AMD"
			g.Temp = gpuTempHwmon(devPath)
			if d, err := os.ReadFile(filepath.Join(devPath, "gpu_busy_percent")); err == nil {
				g.Usage, _ = strconv.ParseFloat(strings.TrimSpace(string(d)), 64)
			}
			readU64 := func(f string) uint64 {
				d, _ := os.ReadFile(filepath.Join(devPath, f))
				v, _ := strconv.ParseUint(strings.TrimSpace(string(d)), 10, 64)
				return v / (1024 * 1024)
			}
			g.VRAMTotal = readU64("mem_info_vram_total")
			g.VRAMUsed = readU64("mem_info_vram_used")
		case "0x8086": // Intel
			g.Vendor = "Intel"
			g.Temp = gpuTempHwmon(devPath)
		default:
			continue
		}
		if g.Temp > 0 || g.Usage > 0 {
			out = append(out, g)
		}
	}
	return out
}

// ── Disks ─────────────────────────────────────────────────────────────────────

var skipFSTypes = map[string]bool{
	"overlay": true, "tmpfs": true, "devtmpfs": true, "squashfs": true,
	"ramfs": true, "sysfs": true, "proc": true, "devpts": true,
	"cgroup": true, "cgroup2": true, "pstore": true, "bpf": true,
	"tracefs": true, "debugfs": true, "securityfs": true, "fusectl": true,
	"hugetlbfs": true, "mqueue": true, "autofs": true, "rpc_pipefs": true,
}

var skipMountPrefixes = []string{
	"/proc", "/sys", "/dev", "/run", "/host", "/snap",
}

func parseMounts() [][3]string {
	data, _ := os.ReadFile(filepath.Join(hostProc, "mounts"))
	var out [][3]string
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 {
			out = append(out, [3]string{f[0], f[1], f[2]})
		}
	}
	return out
}

func isBlockDevice(device string) bool {
	return strings.HasPrefix(device, "/dev/")
}

func skipMount(mount string) bool {
	for _, p := range skipMountPrefixes {
		if strings.HasPrefix(mount, p) {
			return true
		}
	}
	return false
}

// isDisplayMount — only show / and single-level mounts for disk cards
// This filters out all container bind mounts (files)
func isDisplayMount(mount string) bool {
	if skipMount(mount) {
		return false
	}
	// Must be a directory
	info, err := os.Stat(mount)
	if err != nil || !info.IsDir() {
		return false
	}
	// Only root or single component mounts: /, /data, /home — not /config/file.json
	trimmed := strings.Trim(mount, "/")
	if strings.Contains(trimmed, "/") {
		return false
	}
	// No dots in mount name
	if strings.Contains(filepath.Base(mount), ".") {
		return false
	}
	return true
}

func readDisks() []DiskStat {
	mounts := parseMounts()
	seen := map[string]bool{}
	var out []DiskStat

	for _, m := range mounts {
		device, mount, fstype := m[0], m[1], m[2]
		if !isBlockDevice(device) || skipFSTypes[fstype] {
			continue
		}
		if !isDisplayMount(mount) {
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

		if total < 256*1024*1024 { // skip tiny partitions < 256MB
			continue
		}

		// Clean label: device name only, with mount hint for non-root
		dev := filepath.Base(device)
		label := dev
		if mount != "/" {
			label = dev + " " + mount
		}

		out = append(out, DiskStat{
			Device:  dev,
			Mount:   label,
			TotalGB: total / (1024 * 1024 * 1024),
			UsedGB:  used / (1024 * 1024 * 1024),
			FreeGB:  free / (1024 * 1024 * 1024),
			Percent: math.Round(float64(used)*100/float64(total)*10) / 10,
		})
	}
	return out
}

// ── Storage info (type + size for system section) ─────────────────────────────

type StorageDevice struct {
	Name   string `json:"name"`
	Type   string `json:"type"`  // NVMe, SSD, HDD, eMMC, SD
	SizeGB uint64 `json:"size_gb"`
}

func detectStorageDevices() []StorageDevice {
	// Read /sys/block for all block devices
	blockPath := filepath.Join(hostSys, "block")
	entries, err := os.ReadDir(blockPath)
	if err != nil {
		return nil
	}

	var out []StorageDevice
	for _, e := range entries {
		name := e.Name()

		// Skip loop, ram, dm devices
		if strings.HasPrefix(name, "loop") ||
			strings.HasPrefix(name, "ram") ||
			strings.HasPrefix(name, "dm-") ||
			strings.HasPrefix(name, "zram") {
			continue
		}

		devPath := filepath.Join(blockPath, name)

		// Get size in sectors (512 bytes each)
		sizeData, err := os.ReadFile(filepath.Join(devPath, "size"))
		if err != nil {
			continue
		}
		sectors, _ := strconv.ParseUint(strings.TrimSpace(string(sizeData)), 10, 64)
		sizeGB := sectors * 512 / (1024 * 1024 * 1024)
		if sizeGB == 0 {
			continue
		}

		// Detect storage type
		storageType := detectStorageType(devPath, name)

		out = append(out, StorageDevice{
			Name:   name,
			Type:   storageType,
			SizeGB: sizeGB,
		})
	}
	return out
}

func detectStorageType(devPath, name string) string {
	// NVMe — device name starts with nvme
	if strings.HasPrefix(name, "nvme") {
		return "NVMe"
	}

	// eMMC — device name starts with mmcblk
	if strings.HasPrefix(name, "mmcblk") {
		// Check if it's eMMC or SD card
		typeFile := filepath.Join(devPath, "device", "type")
		if data, err := os.ReadFile(typeFile); err == nil {
			t := strings.TrimSpace(string(data))
			if t == "MMC" {
				return "eMMC"
			}
			return "SD"
		}
		return "eMMC"
	}

	// SATA/USB — check rotational flag
	rotData, err := os.ReadFile(filepath.Join(devPath, "queue", "rotational"))
	if err == nil {
		rot := strings.TrimSpace(string(rotData))
		if rot == "0" {
			return "SSD"
		}
		if rot == "1" {
			return "HDD"
		}
	}

	// Check if USB
	if _, err := os.Stat(filepath.Join(devPath, "device", "idVendor")); err == nil {
		return "USB"
	}

	return "Storage"
}

func formatStorageSummary(devices []StorageDevice) string {
	if len(devices) == 0 {
		return ""
	}
	var parts []string
	for _, d := range devices {
		if d.SizeGB >= 1024 {
			parts = append(parts, fmt.Sprintf("%s %.1fTB", d.Type, float64(d.SizeGB)/1024))
		} else {
			parts = append(parts, fmt.Sprintf("%s %dGB", d.Type, d.SizeGB))
		}
	}
	return strings.Join(parts, " · ")
}

// ── Arch detection ────────────────────────────────────────────────────────────

func detectArch() string {
	data, _ := os.ReadFile(filepath.Join(hostProc, "cpuinfo"))
	content := string(data)

	// RISC-V
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "isa") {
			continue
		}
		isa := strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		s := "rv64"
		if strings.Contains(isa, "imafd") {
			s += "g"
		}
		if strings.Contains(isa, "c") {
			s += "c"
		}
		if strings.Contains(isa, "v") {
			s += "v"
		}
		arch := "RISC-V " + s
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
			if strings.Contains(line, " lm") {
				return "x86-64"
			}
			return "x86"
		}
	}
	return ""
}

// ── Hardware auto-detection ───────────────────────────────────────────────────

func isValidBoardName(s string) bool {
	if s == "" {
		return false
	}
	bad := []string{
		"To be filled by O.E.M.", "Default string", "None",
		"Not Applicable", "N/A", "System Product Name",
		"System Version", "Base Board Product Name",
	}
	for _, b := range bad {
		if strings.EqualFold(s, b) {
			return false
		}
	}
	// Skip storage-device-style IDs
	if strings.Contains(s, "[") && strings.Contains(s, "]") {
		return false
	}
	return true
}

func detectBoard() string {
	// x86 DMI
	for _, name := range []string{"product_name", "board_name"} {
		data, err := os.ReadFile(filepath.Join(hostSys, "class", "dmi", "id", name))
		if err == nil {
			if v := strings.TrimSpace(string(data)); isValidBoardName(v) {
				return v
			}
		}
	}
	// ARM/RISC-V device tree
	for _, p := range []string{
		filepath.Join(hostProc, "device-tree", "model"),
		filepath.Join(hostSys, "firmware", "devicetree", "base", "model"),
	} {
		data, err := os.ReadFile(p)
		if err == nil {
			v := strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", ""))
			if v != "" {
				return v
			}
		}
	}
	// cpuinfo Hardware (older ARM)
	data, _ := os.ReadFile(filepath.Join(hostProc, "cpuinfo"))
	for _, line := range strings.Split(string(data), "\n") {
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
	var model string
	cores := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "model name") && model == "" {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				model = strings.TrimSpace(parts[1])
			}
		}
		if strings.HasPrefix(line, "processor") {
			cores++
		}
	}
	if model == "" {
		return ""
	}
	if cores > 0 {
		return fmt.Sprintf("%s · %d cores", model, cores)
	}
	return model
}

func detectRAM() string {
	data, _ := os.ReadFile(filepath.Join(hostProc, "meminfo"))
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		gb := float64(kb) / (1024 * 1024)
		if gb >= 1 {
			return fmt.Sprintf("%.0f GB", gb)
		}
		return fmt.Sprintf("%.1f GB", gb)
	}
	return ""
}

func detectOS() string {
	for _, p := range []string{
		envOr("HOST_OS_RELEASE", "/host/etc/os-release"),
		"/host/proc/1/root/etc/os-release",
		"/etc/os-release",
	} {
		data, err := os.ReadFile(p)
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
	data, _ := os.ReadFile(filepath.Join(hostProc, "version"))
	f := strings.Fields(string(data))
	if len(f) >= 3 {
		return f[2]
	}
	return ""
}

func mergeHardware(h *HardwareConfig) {
	if strings.TrimSpace(h.Board) == "" {
		h.Board = detectBoard()
	}
	if strings.TrimSpace(h.CPU) == "" {
		h.CPU = detectCPU()
	}
	if strings.TrimSpace(h.RAM) == "" {
		h.RAM = detectRAM()
	}
	if strings.TrimSpace(h.OS) == "" {
		h.OS = detectOS()
	}
	if strings.TrimSpace(h.Kernel) == "" {
		h.Kernel = detectKernel()
	}
	// Storage: always auto-detect type and size from block devices
	if strings.TrimSpace(h.Storage) == "" {
		devices := detectStorageDevices()
		h.Storage = formatStorageSummary(devices)
	}
	log.Printf("hardware detected — board:%q cpu:%q ram:%q storage:%q",
		h.Board, h.CPU, h.RAM, h.Storage)
}

// ── JSON helpers ──────────────────────────────────────────────────────────────

// stripComments removes all keys prefixed with _ at any nesting depth
func stripComments(data []byte) []byte {
	// A bare JSON null unmarshals successfully into a map or slice in Go
	// (as nil/empty), which would otherwise cause it to be silently
	// rewritten as {} or [] below. Preserve it as-is.
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" {
		return data
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err == nil && obj != nil {
		clean := make(map[string]json.RawMessage)
		for k, v := range obj {
			if strings.HasPrefix(k, "_") {
				continue
			}
			clean[k] = stripComments(v)
		}
		if b, err := json.Marshal(clean); err == nil {
			return b
		}
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err == nil && arr != nil {
		for i, v := range arr {
			arr[i] = stripComments(v)
		}
		if b, err := json.Marshal(arr); err == nil {
			return b
		}
	}
	return data
}

// ── Stats collector ───────────────────────────────────────────────────────────

type StatsCollector struct {
	mu     sync.RWMutex
	latest StatsResponse
	prev   cpuSample
	arch   string
	cfg    *DashboardConfig
}

func newCollector(cfg *DashboardConfig) *StatsCollector {
	s := &StatsCollector{cfg: cfg}
	s.arch = detectArch()
	if cfg != nil && strings.TrimSpace(cfg.Hardware.Arch) != "" {
		s.arch = cfg.Hardware.Arch
	}
	s.prev, _ = readCPUSample()
	return s
}

func (s *StatsCollector) collect() {
	curr, err := readCPUSample()
	var cpu float64
	if err == nil {
		dt := curr.total - s.prev.total
		di := curr.idle - s.prev.idle
		if dt > 0 {
			cpu = math.Round(float64(dt-di)*100/float64(dt)*10) / 10
		}
		s.prev = curr
	}

	ramUsed, ramTotal, ramPct := readRAM()
	l1, l5, l15 := readLoadAvg()

	var zones []ThermalZone
	if s.cfg != nil {
		zones = s.cfg.Thermal.Zones
	}

	s.mu.Lock()
	s.latest = StatsResponse{
		CPUPercent:    cpu,
		RAMPercent:    ramPct,
		RAMUsedMB:     ramUsed,
		RAMTotalMB:    ramTotal,
		Temps:         readTemps(zones),
		Disks:         readDisks(),
		GPUs:          readGPUs(),
		Load1m:        l1,
		Load5m:        l5,
		Load15m:       l15,
		UptimeSeconds: readUptime(),
		Arch:          s.arch,
		Timestamp:     time.Now().Unix(),
	}
	s.mu.Unlock()
}

func (s *StatsCollector) get() StatsResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latest
}

func (s *StatsCollector) run(d time.Duration) {
	s.collect()
	for range time.NewTicker(d).C {
		s.collect()
	}
}

// ── Status checker ────────────────────────────────────────────────────────────

type StatusChecker struct {
	mu       sync.RWMutex
	latest   StatusResponse
	cfgDir   string
	client   *http.Client
	failures map[string]int    // consecutive failures per URL
	shown    map[string]string // state currently shown per URL
}

func newChecker(dir string) *StatusChecker {
	return &StatusChecker{
		cfgDir:   dir,
		failures: map[string]int{},
		shown:    map[string]string{},
		client: &http.Client{
			Timeout: 8 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
				DisableKeepAlives: true,
			},
			// Do not follow redirects: any HTTP response means the service is up.
			// Following them can land on another slow host and cause false "offline".
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// probeTarget pairs the URL shown/clicked in the UI with the URL actually
// dialed for the online/offline check. They differ when a service sets
// check_url — typically a Docker-network address (http://container:port)
// used to avoid hairpinning back through the LAN/reverse-proxy from
// inside the container, which commonly times out even when the service
// is perfectly reachable from a browser on the LAN.
type probeTarget struct {
	Display string
	Check   string
}

func (sc *StatusChecker) targets() []probeTarget {
	data, err := os.ReadFile(filepath.Join(sc.cfgDir, "services.json"))
	if err != nil {
		return nil
	}
	var cfg struct {
		Servers []struct {
			Services []struct {
				URL      string `json:"url"`
				CheckURL string `json:"check_url"`
				Disabled bool   `json:"disabled"`
			} `json:"services"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(stripComments(data), &cfg); err != nil {
		return nil
	}
	var out []probeTarget
	for _, srv := range cfg.Servers {
		for _, svc := range srv.Services {
			if svc.Disabled || svc.URL == "" {
				continue
			}
			check := strings.TrimSpace(svc.CheckURL)
			if check == "" {
				check = svc.URL
			}
			out = append(out, probeTarget{Display: svc.URL, Check: check})
		}
	}
	return out
}

// probe returns nil if the URL answered with any HTTP response.
// It retries once, which absorbs one-off DNS or connection hiccups.
func (sc *StatusChecker) probe(url string) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			cancel()
			return err
		}
		resp, err := sc.client.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			return nil
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	return lastErr
}

func (sc *StatusChecker) check() {
	targets := sc.targets()
	type result struct {
		url string
		err error
	}
	ch := make(chan result, len(targets))
	for _, t := range targets {
		go func(t probeTarget) { ch <- result{t.Display, sc.probe(t.Check)} }(t)
	}

	res := make(map[string]string, len(targets))
	for range targets {
		r := <-ch
		state := "online"
		if r.err != nil {
			sc.failures[r.url]++
			// Only flip online -> offline after 2 consecutive failed cycles
			if sc.shown[r.url] == "online" && sc.failures[r.url] < 2 {
				state = "online"
			} else {
				state = "offline"
			}
		} else {
			sc.failures[r.url] = 0
		}
		if prev := sc.shown[r.url]; prev != state {
			if r.err != nil {
				log.Printf("status: %s %s -> %s (%v)", r.url, orDash(prev), state, r.err)
			} else {
				log.Printf("status: %s %s -> %s", r.url, orDash(prev), state)
			}
		}
		sc.shown[r.url] = state
		res[r.url] = state
	}

	sc.mu.Lock()
	sc.latest = StatusResponse{Timestamp: time.Now().Unix(), Services: res}
	sc.mu.Unlock()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (sc *StatusChecker) get() StatusResponse {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return sc.latest
}

func (sc *StatusChecker) run(d time.Duration) {
	sc.check()
	for range time.NewTicker(d).C {
		sc.check()
	}
}

// ── HTTP ──────────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(v)
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	log.SetFlags(log.Ltime | log.Lmsgprefix)
	log.SetPrefix("hs-dashboard ")
	log.Printf("starting — port:%s proc:%s sys:%s config:%s",
		listenPort, hostProc, hostSys, configDir)

	// Default config
	cfg := &DashboardConfig{}
	cfg.Display.Theme = "auto"
	cfg.Display.Scale = 1.2
	cfg.Display.Timezone = "UTC"
	cfg.Display.RefreshStatsMs = 5000
	cfg.Display.RefreshStatusMs = 30000
	cfg.Display.TileWidth = 200
	cfg.Logo.Height = 48

	// Load user config
	if raw, err := os.ReadFile(filepath.Join(configDir, "config.json")); err != nil {
		log.Printf("no config.json — using auto-detected values")
	} else if err := json.Unmarshal(stripComments(raw), cfg); err != nil {
		log.Printf("config.json error: %v — using defaults", err)
	} else {
		log.Printf("config loaded: %s (%s)", cfg.Server.Name, cfg.Server.FQDN)
	}

	// Auto-detect hardware (config values override)
	mergeHardware(&cfg.Hardware)

	// Start background workers
	collector := newCollector(cfg)
	go collector.run(time.Duration(statsInterval) * time.Second)

	checker := newChecker(configDir)
	go checker.run(time.Duration(statusInterval) * time.Second)

	// Embedded static files
	wwwFS, err := fs.Sub(staticFiles, "www")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()

	// Serve merged config (auto-detected values fill null fields)
	mux.HandleFunc("/config.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, cfg)
	})

	// Serve services.json with comments stripped
	mux.HandleFunc("/services.json", func(w http.ResponseWriter, r *http.Request) {
		data, err := os.ReadFile(filepath.Join(configDir, "services.json"))
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(stripComments(data))
	})

	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, collector.get())
	})

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, checker.get())
	})

	mux.Handle("/", http.FileServer(http.FS(wwwFS)))

	log.Printf("ready → http://0.0.0.0:%s", listenPort)
	log.Fatal(http.ListenAndServe(":"+listenPort, mux))
}
