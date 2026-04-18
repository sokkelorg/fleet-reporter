package system

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type CoreUsage struct {
	Core    int     `json:"core"`
	UsePct  float64 `json:"use_pct"`
	UserPct float64 `json:"user_pct"`
	SysPct  float64 `json:"sys_pct"`
	IdlePct float64 `json:"idle_pct"`
}

type Metrics struct {
	Hostname      string      `json:"hostname"`
	OS            string      `json:"os"`
	Arch          string      `json:"arch"`
	NumCPU        int         `json:"num_cpu"`
	LoadAvg1      float64     `json:"load_avg_1"`
	LoadAvg5      float64     `json:"load_avg_5"`
	LoadAvg15     float64     `json:"load_avg_15"`
	Cores         []CoreUsage `json:"cores"`
	MemTotalBytes uint64      `json:"mem_total_bytes"`
	MemUsedBytes  uint64      `json:"mem_used_bytes"`
	MemUsedPct    float64     `json:"mem_used_pct"`
}

func Collect() (*Metrics, error) {
	m := &Metrics{
		Hostname: resolveHostname(),
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		NumCPU:   runtime.NumCPU(),
	}

	if err := collectLoadAvg(m); err != nil {
		return nil, err
	}
	if err := collectMemory(m); err != nil {
		return nil, err
	}
	if err := collectCPUPerCore(m); err != nil {
		return nil, err
	}

	return m, nil
}

// resolveHostname prefers an explicit HOSTNAME_OVERRIDE env var, then a
// host-mounted /host/etc/hostname (for containers), then the syscall hostname.
func resolveHostname() string {
	if v := strings.TrimSpace(os.Getenv("HOSTNAME_OVERRIDE")); v != "" {
		return v
	}
	if data, err := os.ReadFile("/host/etc/hostname"); err == nil {
		if v := strings.TrimSpace(string(data)); v != "" {
			return v
		}
	}
	h, _ := os.Hostname()
	return h
}

func collectLoadAvg(m *Metrics) error {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		// fallback: try reading /proc/loadavg (linux)
		return collectLoadAvgProc(m)
	}
	// macOS format: "{ 1.23 4.56 7.89 }"
	s := strings.Trim(string(out), "{ }\n")
	fields := strings.Fields(s)
	if len(fields) >= 3 {
		m.LoadAvg1, _ = strconv.ParseFloat(fields[0], 64)
		m.LoadAvg5, _ = strconv.ParseFloat(fields[1], 64)
		m.LoadAvg15, _ = strconv.ParseFloat(fields[2], 64)
	}
	return nil
}

func collectLoadAvgProc(m *Metrics) error {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return err
	}
	fields := strings.Fields(string(data))
	if len(fields) >= 3 {
		m.LoadAvg1, _ = strconv.ParseFloat(fields[0], 64)
		m.LoadAvg5, _ = strconv.ParseFloat(fields[1], 64)
		m.LoadAvg15, _ = strconv.ParseFloat(fields[2], 64)
	}
	return nil
}

func collectMemory(m *Metrics) error {
	switch runtime.GOOS {
	case "darwin":
		return collectMemoryDarwin(m)
	default:
		return collectMemoryLinux(m)
	}
}

func collectMemoryDarwin(m *Metrics) error {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return err
	}
	m.MemTotalBytes, _ = strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)

	out, err = exec.Command("vm_stat").Output()
	if err != nil {
		return err
	}

	pageSize := uint64(16384) // default arm64
	if ps, err := exec.Command("sysctl", "-n", "hw.pagesize").Output(); err == nil {
		pageSize, _ = strconv.ParseUint(strings.TrimSpace(string(ps)), 10, 64)
	}

	var free, inactive uint64
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Pages free:") {
			free = parseVmStatValue(line) * pageSize
		} else if strings.HasPrefix(line, "Pages inactive:") {
			inactive = parseVmStatValue(line) * pageSize
		}
	}

	available := free + inactive
	if m.MemTotalBytes > available {
		m.MemUsedBytes = m.MemTotalBytes - available
	}
	if m.MemTotalBytes > 0 {
		m.MemUsedPct = float64(m.MemUsedBytes) / float64(m.MemTotalBytes) * 100
	}
	return nil
}

func parseVmStatValue(line string) uint64 {
	parts := strings.SplitN(line, ":", 2)
	if len(parts) < 2 {
		return 0
	}
	s := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(parts[1]), "."))
	v, _ := strconv.ParseUint(s, 10, 64)
	return v
}

func collectCPUPerCore(m *Metrics) error {
	switch runtime.GOOS {
	case "linux":
		return collectCPUPerCoreLinux(m)
	default:
		return collectCPUPerCoreDarwin(m)
	}
}

// Samples /proc/stat twice with a 200ms interval to compute per-core usage.
func collectCPUPerCoreLinux(m *Metrics) error {
	first, err := readProcStat()
	if err != nil {
		return err
	}
	time.Sleep(200 * time.Millisecond)
	second, err := readProcStat()
	if err != nil {
		return err
	}

	for i, s1 := range first {
		if i >= len(second) {
			break
		}
		s2 := second[i]
		user := s2.user - s1.user
		sys := s2.system - s1.system
		idle := s2.idle - s1.idle
		total := user + sys + idle + (s2.other - s1.other)
		if total == 0 {
			m.Cores = append(m.Cores, CoreUsage{Core: i})
			continue
		}
		m.Cores = append(m.Cores, CoreUsage{
			Core:    i,
			UserPct: float64(user) / float64(total) * 100,
			SysPct:  float64(sys) / float64(total) * 100,
			IdlePct: float64(idle) / float64(total) * 100,
			UsePct:  float64(total-idle) / float64(total) * 100,
		})
	}
	return nil
}

type cpuSample struct {
	user, system, idle, other uint64
}

func readProcStat() ([]cpuSample, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil, err
	}
	var samples []cpuSample
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu") || strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		user, _ := strconv.ParseUint(fields[1], 10, 64)
		nice, _ := strconv.ParseUint(fields[2], 10, 64)
		system, _ := strconv.ParseUint(fields[3], 10, 64)
		idle, _ := strconv.ParseUint(fields[4], 10, 64)
		var other uint64
		for _, f := range fields[5:] {
			v, _ := strconv.ParseUint(f, 10, 64)
			other += v
		}
		samples = append(samples, cpuSample{
			user:   user + nice,
			system: system,
			idle:   idle,
			other:  other,
		})
	}
	return samples, nil
}

func collectCPUPerCoreDarwin(m *Metrics) error {
	// Use top in logging mode for one sample
	out, err := exec.Command("top", "-l", "2", "-n", "0", "-stats", "cpu").Output()
	if err != nil {
		return fmt.Errorf("top: %w", err)
	}
	// top -l 2 gives two samples; parse per-core from the last "CPU usage" line
	// Fallback: just report load averages divided across cores (already in loadavg fields)
	// macOS doesn't easily expose per-core usage without IOKit, so we use mpstat-style parsing
	_ = out
	// On macOS, per-core CPU isn't easily available without cgo/IOKit.
	// Leave cores empty on macOS — load averages are still reported.
	return nil
}

func collectMemoryLinux(m *Metrics) error {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return err
	}
	info := make(map[string]uint64)
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) < 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		valStr := strings.TrimSpace(parts[1])
		valStr = strings.TrimSuffix(valStr, " kB")
		v, _ := strconv.ParseUint(strings.TrimSpace(valStr), 10, 64)
		info[key] = v * 1024
	}
	m.MemTotalBytes = info["MemTotal"]
	avail := info["MemAvailable"]
	if m.MemTotalBytes > avail {
		m.MemUsedBytes = m.MemTotalBytes - avail
	}
	if m.MemTotalBytes > 0 {
		m.MemUsedPct = float64(m.MemUsedBytes) / float64(m.MemTotalBytes) * 100
	}
	return nil
}
