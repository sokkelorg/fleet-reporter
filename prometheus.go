package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/sokkelorg/fleet-reporter/storage"
	"github.com/sokkelorg/fleet-reporter/system"
)

func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handlePrometheus(w, r)
	case http.MethodPost:
		s.handlePostMetrics(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) handlePrometheus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	sysRec, _ := s.store.LatestSystem()
	if sysRec != nil {
		var m system.Metrics
		if err := json.Unmarshal(sysRec.Payload, &m); err == nil {
			writeSystemMetrics(w, &m, sysRec.ReportedAt.Unix())
		}
	}

	sims, _ := s.store.LatestSimulationPerService()
	writeSimulationMetrics(w, sims)

	if size, err := s.store.DBSize(); err == nil {
		writeMetric(w, promMetric{
			Name: "fleet_reporter_db_size_bytes",
			Help: "Size of the SQLite database file in bytes.",
			Type: "gauge",
			Samples: []promSample{{Value: float64(size)}},
		})
	}
}

func writeSystemMetrics(w io.Writer, m *system.Metrics, collectedAt int64) {
	host := map[string]string{"hostname": m.Hostname}

	writeMetric(w, promMetric{
		Name: "fleet_reporter_system_info",
		Help: "Static information about the reporting host.",
		Type: "gauge",
		Samples: []promSample{{
			Labels: map[string]string{
				"hostname": m.Hostname,
				"os":       m.OS,
				"arch":     m.Arch,
			},
			Value: 1,
		}},
	})

	writeMetric(w, promMetric{
		Name:    "fleet_reporter_system_cpu_count",
		Help:    "Number of logical CPU cores.",
		Type:    "gauge",
		Samples: []promSample{{Labels: host, Value: float64(m.NumCPU)}},
	})

	writeMetric(w, promMetric{
		Name: "fleet_reporter_system_load_average",
		Help: "System load average over the given window in minutes.",
		Type: "gauge",
		Samples: []promSample{
			{Labels: mergeLabels(host, map[string]string{"window": "1m"}), Value: m.LoadAvg1},
			{Labels: mergeLabels(host, map[string]string{"window": "5m"}), Value: m.LoadAvg5},
			{Labels: mergeLabels(host, map[string]string{"window": "15m"}), Value: m.LoadAvg15},
		},
	})

	writeMetric(w, promMetric{
		Name:    "fleet_reporter_system_memory_total_bytes",
		Help:    "Total physical memory in bytes.",
		Type:    "gauge",
		Samples: []promSample{{Labels: host, Value: float64(m.MemTotalBytes)}},
	})
	writeMetric(w, promMetric{
		Name:    "fleet_reporter_system_memory_used_bytes",
		Help:    "Used physical memory in bytes.",
		Type:    "gauge",
		Samples: []promSample{{Labels: host, Value: float64(m.MemUsedBytes)}},
	})
	writeMetric(w, promMetric{
		Name:    "fleet_reporter_system_memory_used_ratio",
		Help:    "Used memory as a fraction of total (0-1).",
		Type:    "gauge",
		Samples: []promSample{{Labels: host, Value: m.MemUsedPct / 100}},
	})

	if len(m.Cores) > 0 {
		modes := []struct {
			suffix string
			help   string
			pick   func(system.CoreUsage) float64
		}{
			{"use", "Per-core CPU utilization as a fraction (0-1).", func(c system.CoreUsage) float64 { return c.UsePct / 100 }},
			{"user", "Per-core CPU time in user mode as a fraction (0-1).", func(c system.CoreUsage) float64 { return c.UserPct / 100 }},
			{"system", "Per-core CPU time in system mode as a fraction (0-1).", func(c system.CoreUsage) float64 { return c.SysPct / 100 }},
			{"idle", "Per-core CPU idle time as a fraction (0-1).", func(c system.CoreUsage) float64 { return c.IdlePct / 100 }},
		}
		for _, mode := range modes {
			samples := make([]promSample, 0, len(m.Cores))
			for _, c := range m.Cores {
				samples = append(samples, promSample{
					Labels: mergeLabels(host, map[string]string{"core": strconv.Itoa(c.Core)}),
					Value:  mode.pick(c),
				})
			}
			writeMetric(w, promMetric{
				Name:    "fleet_reporter_system_cpu_core_" + mode.suffix + "_ratio",
				Help:    mode.help,
				Type:    "gauge",
				Samples: samples,
			})
		}
	}

	writeMetric(w, promMetric{
		Name:    "fleet_reporter_system_last_collected_timestamp_seconds",
		Help:    "Unix timestamp of the most recent system metrics sample.",
		Type:    "gauge",
		Samples: []promSample{{Labels: host, Value: float64(collectedAt)}},
	})
}

func writeSimulationMetrics(w io.Writer, records []storage.Record) {
	if len(records) == 0 {
		return
	}

	type parsed struct {
		service    map[string]string
		totalFails *float64
		reportedAt int64
	}

	runs := make([]parsed, 0, len(records))
	for _, rec := range records {
		var p struct {
			Version struct {
				Service   string `json:"service"`
				Commit    string `json:"commit"`
				Branch    string `json:"branch"`
				BuildTime string `json:"build_time"`
				GoVersion string `json:"go_version"`
			} `json:"version"`
			TotalFails *float64 `json:"total_fails"`
		}
		_ = json.Unmarshal(rec.Payload, &p)
		runs = append(runs, parsed{
			service: map[string]string{
				"service":    p.Version.Service,
				"commit":     p.Version.Commit,
				"branch":     p.Version.Branch,
				"build_time": p.Version.BuildTime,
				"go_version": p.Version.GoVersion,
			},
			totalFails: p.TotalFails,
			reportedAt: rec.ReportedAt.Unix(),
		})
	}

	infoSamples := make([]promSample, 0, len(runs))
	for _, r := range runs {
		infoSamples = append(infoSamples, promSample{Labels: r.service, Value: 1})
	}
	writeMetric(w, promMetric{
		Name:    "fleet_reporter_simulation_info",
		Help:    "Information about the most recently reported simulation run per service.",
		Type:    "gauge",
		Samples: infoSamples,
	})

	failSamples := make([]promSample, 0, len(runs))
	for _, r := range runs {
		if r.totalFails == nil {
			continue
		}
		failSamples = append(failSamples, promSample{Labels: r.service, Value: *r.totalFails})
	}
	if len(failSamples) > 0 {
		writeMetric(w, promMetric{
			Name:    "fleet_reporter_simulation_total_fails",
			Help:    "Number of failures in the most recently reported simulation run per service.",
			Type:    "gauge",
			Samples: failSamples,
		})
	}

	tsSamples := make([]promSample, 0, len(runs))
	for _, r := range runs {
		tsSamples = append(tsSamples, promSample{
			Labels: map[string]string{"service": r.service["service"]},
			Value:  float64(r.reportedAt),
		})
	}
	writeMetric(w, promMetric{
		Name:    "fleet_reporter_simulation_last_report_timestamp_seconds",
		Help:    "Unix timestamp of the most recently reported simulation run per service.",
		Type:    "gauge",
		Samples: tsSamples,
	})
}

type promMetric struct {
	Name, Help, Type string
	Samples          []promSample
}

type promSample struct {
	Labels map[string]string
	Value  float64
}

func writeMetric(w io.Writer, m promMetric) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", m.Name, escapeHelp(m.Help), m.Name, m.Type)
	for _, s := range m.Samples {
		fmt.Fprintf(w, "%s%s %s\n", m.Name, formatLabels(s.Labels), formatFloat(s.Value))
	}
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, k, escapeLabelValue(labels[k])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

func escapeHelp(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func mergeLabels(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
