package system

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

)

type Metrics struct {
	Hostname      string  `json:"hostname"`
	OS            string  `json:"os"`
	Arch          string  `json:"arch"`
	NumCPU        int     `json:"num_cpu"`
	LoadAvg1      float64 `json:"load_avg_1"`
	LoadAvg5      float64 `json:"load_avg_5"`
	LoadAvg15     float64 `json:"load_avg_15"`
	MemTotalBytes uint64  `json:"mem_total_bytes"`
	MemUsedBytes  uint64  `json:"mem_used_bytes"`
	MemUsedPct    float64 `json:"mem_used_pct"`
}

func Collect() (*Metrics, error) {
	hostname, _ := os.Hostname()

	m := &Metrics{
		Hostname: hostname,
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

	return m, nil
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
