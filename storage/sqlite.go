package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const maxDBSize = 100 * 1024 * 1024 * 1024 // 100 GB

type Store struct {
	db   *sql.DB
	path string
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS metrics (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			reported_at TEXT NOT NULL,
			payload JSON NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_metrics_reported_at ON metrics(reported_at);

		CREATE TABLE IF NOT EXISTS system_metrics (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			collected_at TEXT NOT NULL,
			payload JSON NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_system_metrics_collected_at ON system_metrics(collected_at);
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create table: %w", err)
	}

	return &Store{db: db, path: path}, nil
}

func (s *Store) Insert(reportedAt time.Time, payload json.RawMessage) error {
	_, err := s.db.Exec(
		"INSERT INTO metrics (reported_at, payload) VALUES (?, ?)",
		reportedAt.Format(time.RFC3339Nano),
		string(payload),
	)
	return err
}

type Record struct {
	ReportedAt time.Time       `json:"reported_at"`
	Payload    json.RawMessage `json:"metrics"`
}

// MetricsFilter holds optional filters for querying the metrics table.
type MetricsFilter struct {
	Since       time.Time
	Last        int
	Commit      string
	Branch      string
	GoVersion   string
	HasFailures bool
}

func (s *Store) QueryMetrics(f MetricsFilter) ([]Record, error) {
	var where []string
	var args []any

	if !f.Since.IsZero() {
		where = append(where, "reported_at >= ?")
		args = append(args, f.Since.Format(time.RFC3339Nano))
	}
	if f.Commit != "" {
		where = append(where, "json_extract(payload, '$.version.commit') = ?")
		args = append(args, f.Commit)
	}
	if f.Branch != "" {
		where = append(where, "json_extract(payload, '$.version.branch') = ?")
		args = append(args, f.Branch)
	}
	if f.GoVersion != "" {
		where = append(where, "json_extract(payload, '$.version.go_version') = ?")
		args = append(args, f.GoVersion)
	}
	if f.HasFailures {
		where = append(where, "json_extract(payload, '$.total_fails') > 0")
	}

	query := "SELECT reported_at, payload FROM metrics"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}

	if f.Last > 0 {
		query += " ORDER BY reported_at DESC LIMIT ?"
		args = append(args, f.Last)
	} else {
		query += " ORDER BY reported_at ASC"
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records, err := scanRecords(rows)
	if err != nil {
		return nil, err
	}

	if f.Last > 0 {
		for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
			records[i], records[j] = records[j], records[i]
		}
	}
	return records, nil
}

// LatestSimulationPerService returns the most recent metrics record for each
// distinct value of $.version.service. Records without a service field are
// grouped under the empty string.
func (s *Store) LatestSimulationPerService() ([]Record, error) {
	rows, err := s.db.Query(`
		SELECT m.reported_at, m.payload
		FROM metrics m
		INNER JOIN (
			SELECT
				COALESCE(json_extract(payload, '$.version.service'), '') AS svc,
				MAX(reported_at) AS max_ts
			FROM metrics
			GROUP BY svc
		) latest
		ON COALESCE(json_extract(m.payload, '$.version.service'), '') = latest.svc
		AND m.reported_at = latest.max_ts
		ORDER BY m.reported_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecords(rows)
}

func (s *Store) InsertSystemMetrics(collectedAt time.Time, payload json.RawMessage) error {
	_, err := s.db.Exec(
		"INSERT INTO system_metrics (collected_at, payload) VALUES (?, ?)",
		collectedAt.Format(time.RFC3339Nano),
		string(payload),
	)
	return err
}

func (s *Store) QuerySystemSince(since time.Time) ([]Record, error) {
	rows, err := s.db.Query(
		"SELECT collected_at, payload FROM system_metrics WHERE collected_at >= ? ORDER BY collected_at ASC",
		since.Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecords(rows)
}

func (s *Store) QuerySystemLast(n int) ([]Record, error) {
	rows, err := s.db.Query(
		"SELECT collected_at, payload FROM system_metrics ORDER BY collected_at DESC LIMIT ?",
		n,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records, err := scanRecords(rows)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}
	return records, nil
}

func (s *Store) LatestSystem() (*Record, error) {
	row := s.db.QueryRow(
		"SELECT collected_at, payload FROM system_metrics ORDER BY collected_at DESC LIMIT 1",
	)
	var ts string
	var payload string
	if err := row.Scan(&ts, &payload); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	t, _ := time.Parse(time.RFC3339Nano, ts)
	return &Record{ReportedAt: t, Payload: json.RawMessage(payload)}, nil
}

// DBSize returns the current database file size in bytes.
func (s *Store) DBSize() (int64, error) {
	info, err := os.Stat(s.path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// Prune deletes the oldest 10% of records from both tables.
func (s *Store) Prune() (int64, error) {
	var total int64
	for _, q := range []struct {
		countSQL  string
		deleteSQL string
	}{
		{
			"SELECT COUNT(*) FROM metrics",
			"DELETE FROM metrics WHERE id IN (SELECT id FROM metrics ORDER BY reported_at ASC LIMIT ?)",
		},
		{
			"SELECT COUNT(*) FROM system_metrics",
			"DELETE FROM system_metrics WHERE id IN (SELECT id FROM system_metrics ORDER BY collected_at ASC LIMIT ?)",
		},
	} {
		var count int64
		if err := s.db.QueryRow(q.countSQL).Scan(&count); err != nil {
			return total, err
		}
		if count == 0 {
			continue
		}
		toDelete := count / 10
		if toDelete < 1 {
			toDelete = 1
		}
		res, err := s.db.Exec(q.deleteSQL, toDelete)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// Vacuum reclaims disk space after deletions.
func (s *Store) Vacuum() error {
	_, err := s.db.Exec("VACUUM")
	return err
}

func (s *Store) Close() error {
	return s.db.Close()
}

// StartCleaner runs a background goroutine that checks the DB size
// periodically and prunes old records when it exceeds maxDBSize.
func (s *Store) StartCleaner(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			size, err := s.DBSize()
			if err != nil {
				log.Printf("cleaner: failed to get db size: %v", err)
				continue
			}
			if size <= maxDBSize {
				continue
			}
			log.Printf("cleaner: db size %d bytes exceeds limit, pruning", size)
			for {
				deleted, err := s.Prune()
				if err != nil {
					log.Printf("cleaner: prune error: %v", err)
					break
				}
				log.Printf("cleaner: pruned %d records", deleted)

				if err := s.Vacuum(); err != nil {
					log.Printf("cleaner: vacuum error: %v", err)
					break
				}
				size, err = s.DBSize()
				if err != nil || size <= maxDBSize {
					break
				}
			}
			if size, err := s.DBSize(); err == nil {
				log.Printf("cleaner: db size after cleanup: %d bytes", size)
			}
		}
	}()
}

func scanRecords(rows *sql.Rows) ([]Record, error) {
	var records []Record
	for rows.Next() {
		var ts string
		var payload string
		if err := rows.Scan(&ts, &payload); err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339Nano, ts)
		records = append(records, Record{
			ReportedAt: t,
			Payload:    json.RawMessage(payload),
		})
	}
	return records, rows.Err()
}
