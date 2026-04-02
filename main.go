package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/sokkelorg/fleet-reporter/storage"
	"github.com/sokkelorg/fleet-reporter/system"
)

type statsResponse struct {
	Data        []storage.Record `json:"data"`
	CollectedAt string           `json:"collected_at"`
}

type server struct {
	store *storage.Store
}

func (s *server) handlePostMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	if !json.Valid(body) {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if err := s.store.Insert(time.Now().UTC(), body); err != nil {
		http.Error(w, "failed to store metrics: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleStatsSimulation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	since := parseSince(q.Get("since"))
	last := parseLast(q.Get("last"))

	var records []storage.Record
	var err error

	switch {
	case !since.IsZero():
		records, err = s.store.QuerySince(since)
	case last > 0:
		records, err = s.store.QueryLast(last)
	default:
		rec, qerr := s.store.Latest()
		err = qerr
		if rec != nil {
			records = []storage.Record{*rec}
		}
	}

	if err != nil {
		http.Error(w, "query error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeStats(w, records)
}

func (s *server) handleStatsSystem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	since := parseSince(q.Get("since"))
	last := parseLast(q.Get("last"))

	var records []storage.Record
	var err error

	switch {
	case !since.IsZero():
		records, err = s.store.QuerySystemSince(since)
	case last > 0:
		records, err = s.store.QuerySystemLast(last)
	default:
		rec, qerr := s.store.LatestSystem()
		err = qerr
		if rec != nil {
			records = []storage.Record{*rec}
		}
	}

	if err != nil {
		http.Error(w, "query error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeStats(w, records)
}

func writeStats(w http.ResponseWriter, records []storage.Record) {
	resp := statsResponse{
		Data:        records,
		CollectedAt: time.Now().UTC().Format(time.RFC3339),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func parseSince(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	// Try RFC3339 first
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t
	}
	// Try relative duration like "30d", "12h", "5m"
	if d, ok := parseRelativeDuration(v); ok {
		return time.Now().UTC().Add(-d)
	}
	return time.Time{}
}

func parseRelativeDuration(v string) (time.Duration, bool) {
	if len(v) < 2 {
		return 0, false
	}
	numStr := v[:len(v)-1]
	unit := v[len(v)-1]
	n, err := strconv.Atoi(numStr)
	if err != nil || n <= 0 {
		return 0, false
	}
	switch unit {
	case 'm':
		return time.Duration(n) * time.Minute, true
	case 'h':
		return time.Duration(n) * time.Hour, true
	case 'd':
		return time.Duration(n) * 24 * time.Hour, true
	case 'w':
		return time.Duration(n) * 7 * 24 * time.Hour, true
	default:
		return 0, false
	}
}

func parseLast(v string) int {
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func startSystemSampler(store *storage.Store, interval time.Duration) {
	go func() {
		sample := func() {
			m, err := system.Collect()
			if err != nil {
				log.Printf("system sampler: %v", err)
				return
			}
			payload, err := json.Marshal(m)
			if err != nil {
				log.Printf("system sampler: marshal: %v", err)
				return
			}
			if err := store.InsertSystemMetrics(time.Now().UTC(), payload); err != nil {
				log.Printf("system sampler: insert: %v", err)
			}
		}
		sample()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			sample()
		}
	}()
}

func main() {
	addr := ":4850"
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		addr = v
	}

	dbPath := "fleet-reporter.db"
	if v := os.Getenv("DB_PATH"); v != "" {
		dbPath = v
	}

	store, err := storage.Open(dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	store.StartCleaner(5 * time.Minute)
	startSystemSampler(store, 30*time.Second)

	s := &server{store: store}

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", s.handlePostMetrics)
	mux.HandleFunc("/stats/system", s.handleStatsSystem)
	mux.HandleFunc("/stats/simulation", s.handleStatsSimulation)

	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
