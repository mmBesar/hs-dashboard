package main

// stats.go — reads CPU, memory, disks, network and temperature.
//
// Inside the container the host's /proc and /sys are mounted at /host/proc and
// /host/sys. On bare metal the same code falls back to the normal /proc and /sys.
// Nothing here is architecture-specific, so it behaves the same on x86, ARM
// boards and RISC-V.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	sampleEvery = 2 * time.Second
	historyLen  = 150 // 150 samples x 2s = last 5 minutes
)

// hostPath prefers /host/<sub> (container) and falls back to /<sub> (bare metal).
func hostPath(sub string) string {
	if _, err := os.Stat("/host/" + sub); err == nil {
		return "/host/" + sub
	}
	return "/" + sub
}

var (
	procDir = hostPath("proc")
	sysDir  = hostPath("sys")
)

func readStr(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.Trim(string(b), "\x00 \r\n\t")
}

func r1(v float64) float64 { return math.Round(v*10) / 10 }

// ---- JSON shapes sent to the browser ----

type Info struct {
	Host   string `json:"host"`
	Model  string `json:"model"`
	OS     string `json:"os"`
	Kernel string `json:"kernel"`
	Arch   string `json:"arch"`
}

type CPU struct {
	Total float64   `json:"total"`
	Cores []float64 `json:"cores"`
}

type Mem struct {
	Total     uint64 `json:"total"`
	Used      uint64 `json:"used"`
	Cached    uint64 `json:"cached"`
	SwapTotal uint64 `json:"swapTotal"`
	SwapUsed  uint64 `json:"swapUsed"`
}

type Net struct {
	Rx float64 `json:"rx"` // bytes per second, download
	Tx float64 `json:"tx"` // bytes per second, upload
}

type Disk struct {
	Mount  string `json:"mount"`
	Device string `json:"device"`
	FS     string `json:"fs"`
	Total  uint64 `json:"total"`
	Used   uint64 `json:"used"`
}

type Snapshot struct {
	T      int64      `json:"t"`
	Uptime float64    `json:"uptime"`
	Load   [3]float64 `json:"load"`
	CPU    CPU        `json:"cpu"`
	Mem    Mem        `json:"mem"`
	Temp   *float64   `json:"temp"` // null when the board has no sensor
	Net    Net        `json:"net"`
	GPUs   []GPU      `json:"gpus"`
	Disks  []Disk     `json:"disks"`
}

// Point is the small record kept for the live graphs.
type Point struct {
	T   int64   `json:"t"`
	CPU float64 `json:"cpu"`
	GPU float64 `json:"gpu"`
	Mem float64 `json:"mem"` // percent
	Rx  float64 `json:"rx"`
	Tx  float64 `json:"tx"`
}

// ---- the sampler ----

type cpuTimes struct{ total, idle uint64 }

type Sampler struct {
	info Info

	mu     sync.RWMutex
	latest Snapshot
	hist   []Point

	prevAll   cpuTimes
	prevCores []cpuTimes
	prevRx    uint64
	prevTx    uint64
	prevT     time.Time

	disks   []Disk
	disksAt time.Time

	gpuDevs   []gpuDev
	gpuAt     time.Time
	gpuStates map[string]*gpuState
}

func newSampler() *Sampler {
	return &Sampler{info: loadInfo(), gpuStates: map[string]*gpuState{},
		latest: Snapshot{Disks: []Disk{}, GPUs: []GPU{}, CPU: CPU{Cores: []float64{}}}}
}

func (s *Sampler) run() {
	s.sample() // first pass only sets the baseline for the CPU and network deltas
	t := time.NewTicker(sampleEvery)
	for range t.C {
		s.sample()
	}
}

func (s *Sampler) Latest() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latest
}

func (s *Sampler) History() []Point {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Point{}, s.hist...)
}

func (s *Sampler) sample() {
	now := time.Now()
	all, cores := readCPU()
	rx, tx := readNet()

	snap := Snapshot{T: now.UnixMilli(), Uptime: readUptime(), Load: readLoad(), Mem: readMem(), Temp: readTemp()}
	snap.CPU.Cores = make([]float64, len(cores))

	s.mu.Lock()
	defer s.mu.Unlock()

	warm := s.prevAll.total != 0
	if warm {
		snap.CPU.Total = cpuPct(s.prevAll, all)
		for i := range cores {
			if i < len(s.prevCores) {
				snap.CPU.Cores[i] = cpuPct(s.prevCores[i], cores[i])
			}
		}
		if dt := now.Sub(s.prevT).Seconds(); dt > 0 {
			if rx >= s.prevRx {
				snap.Net.Rx = r1(float64(rx-s.prevRx) / dt)
			}
			if tx >= s.prevTx {
				snap.Net.Tx = r1(float64(tx-s.prevTx) / dt)
			}
		}
	}
	s.prevAll, s.prevCores, s.prevRx, s.prevTx, s.prevT = all, cores, rx, tx, now

	// Disks barely change, so look at them every 20 seconds instead of every 2.
	if s.disks == nil || now.Sub(s.disksAt) > 20*time.Second {
		s.disks, s.disksAt = readDisks(), now
	}
	snap.Disks = s.disks

	// Graphics chips: look for them once a minute, read their numbers every sample.
	if s.gpuDevs == nil || now.Sub(s.gpuAt) > time.Minute {
		s.gpuDevs, s.gpuAt = discoverGPUs(), now
	}
	snap.GPUs = []GPU{}
	for _, d := range s.gpuDevs {
		st := s.gpuStates[d.key]
		if st == nil {
			st = &gpuState{}
			s.gpuStates[d.key] = st
		}
		if g, ok := d.read(st, now); ok {
			snap.GPUs = append(snap.GPUs, g)
		}
	}
	s.latest = snap

	if warm {
		memPct := 0.0
		if snap.Mem.Total > 0 {
			memPct = r1(100 * float64(snap.Mem.Used) / float64(snap.Mem.Total))
		}
		gpuPct := 0.0
		if len(snap.GPUs) > 0 && snap.GPUs[0].Usage != nil {
			gpuPct = *snap.GPUs[0].Usage
		}
		s.hist = append(s.hist, Point{T: snap.T, CPU: snap.CPU.Total, GPU: gpuPct, Mem: memPct, Rx: snap.Net.Rx, Tx: snap.Net.Tx})
		if len(s.hist) > historyLen {
			s.hist = s.hist[len(s.hist)-historyLen:]
		}
	}
}

// stream pushes a fresh snapshot to the browser every 2 seconds (Server-Sent Events).
func (s *Sampler) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // stops nginx from buffering the stream
	send := func() bool {
		b, _ := json.Marshal(s.Latest())
		_, err := fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
		return err == nil
	}
	if !send() {
		return
	}
	t := time.NewTicker(sampleEvery)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			if !send() {
				return
			}
		}
	}
}

// ---- readers ----

func cpuPct(a, b cpuTimes) float64 {
	if b.total <= a.total {
		return 0
	}
	busy := 1 - float64(b.idle-a.idle)/float64(b.total-a.total)
	return r1(math.Max(0, math.Min(100, busy*100)))
}

// readCPU parses /proc/stat: the first line is all cores, then one line per core.
func readCPU() (cpuTimes, []cpuTimes) {
	f, err := os.Open(procDir + "/stat")
	if err != nil {
		return cpuTimes{}, nil
	}
	defer f.Close()
	var all cpuTimes
	var cores []cpuTimes
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fl := strings.Fields(sc.Text())
		if len(fl) < 5 || !strings.HasPrefix(fl[0], "cpu") {
			continue
		}
		var t cpuTimes
		for i, v := range fl[1:] {
			if i >= 8 { // user nice system idle iowait irq softirq steal
				break
			}
			n, _ := strconv.ParseUint(v, 10, 64)
			t.total += n
			if i == 3 || i == 4 { // idle + iowait count as "not busy"
				t.idle += n
			}
		}
		if fl[0] == "cpu" {
			all = t
		} else {
			cores = append(cores, t)
		}
	}
	return all, cores
}

func readUptime() float64 {
	fl := strings.Fields(readStr(procDir + "/uptime"))
	if len(fl) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fl[0], 64)
	return v
}

func readLoad() (l [3]float64) {
	fl := strings.Fields(readStr(procDir + "/loadavg"))
	for i := 0; i < 3 && i < len(fl); i++ {
		l[i], _ = strconv.ParseFloat(fl[i], 64)
	}
	return
}

func readMem() Mem {
	f, err := os.Open(procDir + "/meminfo")
	if err != nil {
		return Mem{}
	}
	defer f.Close()
	kb := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fl := strings.Fields(sc.Text())
		if len(fl) >= 2 {
			n, _ := strconv.ParseUint(fl[1], 10, 64)
			kb[strings.TrimSuffix(fl[0], ":")] = n * 1024
		}
	}
	total, avail := kb["MemTotal"], kb["MemAvailable"]
	if avail > total {
		avail = total
	}
	return Mem{
		Total:     total,
		Used:      total - avail,
		Cached:    kb["Buffers"] + kb["Cached"],
		SwapTotal: kb["SwapTotal"],
		SwapUsed:  kb["SwapTotal"] - kb["SwapFree"],
	}
}

// readTemp returns the hottest CPU/board sensor in °C. ARM and RISC-V boards usually
// expose thermal_zone*, x86 machines expose hwmon — both are checked. Sensors that
// belong to the GPU, SSDs or wifi cards are skipped (the GPU has its own reading).
func readTemp() *float64 {
	zones, _ := filepath.Glob(sysDir + "/class/thermal/thermal_zone*/temp")
	hwmon, _ := filepath.Glob(sysDir + "/class/hwmon/hwmon*/temp*_input")
	best, found := 0.0, false
	for _, p := range append(zones, hwmon...) {
		dir := filepath.Dir(p)
		name := readStr(dir + "/type") // thermal zones
		if name == "" {
			name = readStr(dir + "/name") // hwmon chips
		}
		if !cpuSensor(name) {
			continue
		}
		v, err := strconv.ParseFloat(readStr(p), 64)
		if err != nil {
			continue
		}
		c := v / 1000
		if c <= 0 || c > 150 {
			continue
		}
		if !found || c > best {
			best, found = c, true
		}
	}
	if !found {
		return nil
	}
	best = r1(best)
	return &best
}

func cpuSensor(name string) bool {
	n := strings.ToLower(name)
	for _, skip := range []string{"gpu", "nvme", "amdgpu", "nouveau", "iwlwifi", "drivetemp", "battery"} {
		if strings.Contains(n, skip) {
			return false
		}
	}
	return true
}

// readNet adds up all real network cards (skips loopback and Docker's virtual ones).
// With network_mode: host this is the host's traffic.
func readNet() (rx, tx uint64) {
	f, err := os.Open(procDir + "/net/dev")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(line[:i])
		skip := false
		for _, p := range []string{"lo", "veth", "docker", "br-", "virbr", "cni", "flannel"} {
			if strings.HasPrefix(name, p) {
				skip = true
			}
		}
		fl := strings.Fields(line[i+1:])
		if skip || len(fl) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(fl[0], 10, 64)
		t, _ := strconv.ParseUint(fl[8], 10, 64)
		rx += r
		tx += t
	}
	return
}

var realFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "btrfs": true, "xfs": true, "f2fs": true,
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "zfs": true, "bcachefs": true,
}

// readDisks finds the host's filesystems. The host root is mounted at /host/root
// (set HOST_ROOT=/ when running on bare metal). Several mounts on one device —
// for example Btrfs subvolumes — are shown once.
func readDisks() []Disk {
	root := strings.TrimRight(env("HOST_ROOT", "/host/root"), "/")
	if root != "" {
		if _, err := os.Stat(root); err != nil {
			return []Disk{}
		}
	}
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return []Disk{}
	}
	defer f.Close()

	byDev := map[string]Disk{}
	var order []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fl := strings.Fields(sc.Text())
		if len(fl) < 3 || !realFS[fl[2]] {
			continue
		}
		dev, mp, fsType := fl[0], fl[1], fl[2]
		if root != "" && mp != root && !strings.HasPrefix(mp, root+"/") {
			continue
		}
		label := strings.TrimPrefix(mp, root)
		if label == "" {
			label = "/"
		}
		var st syscall.Statfs_t
		if syscall.Statfs(mp, &st) != nil {
			continue
		}
		bs := uint64(st.Bsize)
		total := uint64(st.Blocks) * bs
		if total == 0 {
			continue
		}
		d := Disk{Mount: label, Device: strings.TrimPrefix(dev, "/dev/"), FS: fsType,
			Total: total, Used: total - uint64(st.Bfree)*bs}
		if old, seen := byDev[dev]; !seen {
			order = append(order, dev)
			byDev[dev] = d
		} else if len(label) < len(old.Mount) {
			byDev[dev] = d // keep the shortest mount path for each device
		}
	}
	out := []Disk{}
	for _, dev := range order {
		out = append(out, byDev[dev])
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Mount) < len(out[j].Mount) })
	return out
}

// loadInfo gathers the things that never change while running.
func loadInfo() Info {
	host := readStr("/host/hostname") // mount /etc/hostname here, see compose.yaml
	if host == "" {
		host, _ = os.Hostname()
	}

	osName := ""
	for _, line := range strings.Split(readStr("/host/os-release"), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			osName = strings.Trim(v, `"`)
		}
	}

	// Board name: ARM/RISC-V boards publish it in the device tree, x86 in DMI.
	model := readStr(sysDir + "/firmware/devicetree/base/model")
	if model == "" {
		model = strings.TrimSpace(readStr(sysDir+"/devices/virtual/dmi/id/sys_vendor") + " " + readStr(sysDir+"/devices/virtual/dmi/id/product_name"))
	}
	if model == "" {
		for _, line := range strings.Split(readStr(procDir+"/cpuinfo"), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
				model = strings.TrimSpace(v)
				break
			}
		}
	}

	return Info{Host: host, Model: model, OS: osName, Kernel: readStr(procDir + "/sys/kernel/osrelease"), Arch: runtime.GOARCH}
}
