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

type StatusResponse struct {
	Simrunner   []storage.Record `json:"simrunner"`
	System      []storage.Record `json:"system"`
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

func (s *server) handleGetStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	since := parseSince(q.Get("since"))
	last := parseLast(q.Get("last"))

	var simRecords, sysRecords []storage.Record
	var err error

	switch {
	case !since.IsZero():
		simRecords, err = s.store.QuerySince(since)
		if err == nil {
			sysRecords, err = s.store.QuerySystemSince(since)
		}
	case last > 0:
		simRecords, err = s.store.QueryLast(last)
		if err == nil {
			sysRecords, err = s.store.QuerySystemLast(last)
		}
	default:
		rec, qerr := s.store.Latest()
		err = qerr
		if rec != nil {
			simRecords = []storage.Record{*rec}
		}
		if err == nil {
			sysRec, qerr := s.store.LatestSystem()
			err = qerr
			if sysRec != nil {
				sysRecords = []storage.Record{*sysRec}
			}
		}
	}

	if err != nil {
		http.Error(w, "query error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := StatusResponse{
		Simrunner:   simRecords,
		System:      sysRecords,
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
	mux.HandleFunc("/status", s.handleGetStatus)

	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
