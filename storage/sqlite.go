package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
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

func (s *Store) QuerySince(since time.Time) ([]Record, error) {
	rows, err := s.db.Query(
		"SELECT reported_at, payload FROM metrics WHERE reported_at >= ? ORDER BY reported_at ASC",
		since.Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecords(rows)
}

func (s *Store) QueryLast(n int) ([]Record, error) {
	rows, err := s.db.Query(
		"SELECT reported_at, payload FROM metrics ORDER BY reported_at DESC LIMIT ?",
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
	// reverse to chronological order
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}
	return records, nil
}

func (s *Store) Latest() (*Record, error) {
	row := s.db.QueryRow(
		"SELECT reported_at, payload FROM metrics ORDER BY reported_at DESC LIMIT 1",
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

// Prune deletes the oldest 10% of records.
func (s *Store) Prune() (int64, error) {
	var count int64
	err := s.db.QueryRow("SELECT COUNT(*) FROM metrics").Scan(&count)
	if err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}

	toDelete := count / 10
	if toDelete < 1 {
		toDelete = 1
	}

	res, err := s.db.Exec(
		"DELETE FROM metrics WHERE id IN (SELECT id FROM metrics ORDER BY reported_at ASC LIMIT ?)",
		toDelete,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
