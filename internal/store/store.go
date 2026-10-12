// Package store persists samples in SQLite (pure-Go driver) and serves the
// queries the dashboard API needs.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"website-stats/internal/collect"
)

type Store struct {
	db *sql.DB
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "stats.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // sqlite: single writer, avoids busy churn
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, err
		}
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS samples (
			ts        INTEGER NOT NULL,
			site      TEXT NOT NULL,
			cpu       REAL NOT NULL DEFAULT 0,
			mem       REAL NOT NULL DEFAULT 0,
			procs     INTEGER NOT NULL DEFAULT 0,
			req       INTEGER NOT NULL DEFAULT 0,
			bytes     INTEGER NOT NULL DEFAULT 0,
			r4        INTEGER NOT NULL DEFAULT 0,
			r5        INTEGER NOT NULL DEFAULT 0,
			fpm_active INTEGER NOT NULL DEFAULT -1,
			fpm_idle   INTEGER NOT NULL DEFAULT -1,
			fpm_queue  INTEGER NOT NULL DEFAULT 0,
			fpm_req    INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (ts, site)
		)`,
		`CREATE TABLE IF NOT EXISTS disks (
			ts   INTEGER NOT NULL,
			site TEXT NOT NULL,
			bytes INTEGER NOT NULL,
			PRIMARY KEY (ts, site)
		)`,
		`CREATE TABLE IF NOT EXISTS urlstats (
			win  INTEGER NOT NULL,
			site TEXT NOT NULL,
			path TEXT NOT NULL,
			cnt  INTEGER NOT NULL,
			bytes INTEGER NOT NULL,
			PRIMARY KEY (win, site, path)
		)`,
		`CREATE TABLE IF NOT EXISTS ipstats (
			win  INTEGER NOT NULL,
			site TEXT NOT NULL,
			ip   TEXT NOT NULL,
			cnt  INTEGER NOT NULL,
			bytes INTEGER NOT NULL,
			PRIMARY KEY (win, site, ip)
		)`,
		`CREATE TABLE IF NOT EXISTS events (
			ts   INTEGER NOT NULL,
			kind TEXT NOT NULL,
			site TEXT NOT NULL,
			rule TEXT NOT NULL DEFAULT '',
			message TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS events_ts ON events (ts DESC)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token   TEXT PRIMARY KEY,
			created INTEGER NOT NULL
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// InsertMinute persists one batch of per-minute rollups.
func (s *Store) InsertMinute(rows []collect.MinuteRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO samples
		(ts, site, cpu, mem, procs, req, bytes, r4, r5, fpm_active, fpm_idle, fpm_queue, fpm_req)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.Exec(r.Ts, r.Site, r.CPU, r.MemMB, r.Procs, r.Req, r.Bytes, r.R4xx, r.R5xx,
			r.FpmActive, r.FpmIdle, r.FpmQueue, r.FpmReq); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// InsertDisk persists an hourly disk usage point.
func (s *Store) InsertDisk(ts int64, site string, bytes int64) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO disks (ts, site, bytes) VALUES (?,?,?)`, ts, site, bytes)
	return err
}

// InsertTopWindow persists one drained top-URL/IP window.
func (s *Store) InsertTopWindow(w collect.TopWindow) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	u, err := tx.Prepare(`INSERT OR REPLACE INTO urlstats (win, site, path, cnt, bytes) VALUES (?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer u.Close()
	for site, entries := range w.URLs {
		for _, e := range entries {
			if _, err := u.Exec(w.Start, site, e.Value, e.Count, e.Bytes); err != nil {
				return err
			}
		}
	}
	i, err := tx.Prepare(`INSERT OR REPLACE INTO ipstats (win, site, ip, cnt, bytes) VALUES (?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer i.Close()
	for site, entries := range w.IPs {
		for _, e := range entries {
			if _, err := i.Exec(w.Start, site, e.Value, e.Count, e.Bytes); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// InsertEvent logs an alert event.
func (s *Store) InsertEvent(ts int64, kind, site, rule, message string) error {
	_, err := s.db.Exec(`INSERT INTO events (ts, kind, site, rule, message) VALUES (?,?,?,?,?)`,
		ts, kind, site, rule, message)
	return err
}

// CreateSession stores a login session token.
func (s *Store) CreateSession(token string, created int64) error {
	_, err := s.db.Exec(`INSERT INTO sessions (token, created) VALUES (?,?)`, token, created)
	return err
}

// ValidSession reports whether the token exists and is younger than ttl.
func (s *Store) ValidSession(token string, ttlSec int64, now int64) bool {
	var created int64
	err := s.db.QueryRow(`SELECT created FROM sessions WHERE token = ?`, token).Scan(&created)
	return err == nil && now-created < ttlSec
}

// DeleteSession removes a session (logout).
func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// Prune deletes samples older than the configured retention.
func (s *Store) Prune(minuteDays, diskDays, topDays, eventDays int) error {
	q := func(table, col string, days int) error {
		_, err := s.db.Exec(fmt.Sprintf(`DELETE FROM %s WHERE %s < unixepoch('now') - ?`, table, col), int64(days)*86400)
		return err
	}
	if err := q("samples", "ts", minuteDays); err != nil {
		return err
	}
	if err := q("disks", "ts", diskDays); err != nil {
		return err
	}
	if err := q("urlstats", "win", topDays); err != nil {
		return err
	}
	if err := q("ipstats", "win", topDays); err != nil {
		return err
	}
	return q("events", "ts", eventDays)
}
