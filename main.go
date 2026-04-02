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
	System      *system.Metrics  `json:"system"`
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

	sysMetrics, err := system.Collect()
	if err != nil {
		http.Error(w, "failed to collect system metrics: "+err.Error(), http.StatusInternalServerError)
		return
	}

	q := r.URL.Query()
	since := parseSince(q.Get("since"))
	last := parseLast(q.Get("last"))

	var records []storage.Record

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

	resp := StatusResponse{
		Simrunner:   records,
		System:      sysMetrics,
		CollectedAt: time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func parseSince(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}
	}
	return t
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

func main() {
	addr := ":8080"
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

	s := &server{store: store}

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", s.handlePostMetrics)
	mux.HandleFunc("/status", s.handleGetStatus)

	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
