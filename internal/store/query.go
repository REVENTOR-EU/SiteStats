package store

import (
	"database/sql"
	"fmt"
)

// MinutePoint is one (possibly bucketed) sample row from the samples table.
type MinutePoint struct {
	Ts        int64
	Site      string
	CPU       float64
	Mem       float64
	Procs     int64
	Req       int64
	Bytes     int64
	R4        int64
	R5        int64
	FpmActive int64
	FpmReq    int64
}

const minuteCols = `ts, site, cpu, mem, procs, req, bytes, r4, r5, fpm_active, fpm_req`

func scanMinute(rows *sql.Rows) (MinutePoint, error) {
	var p MinutePoint
	var fpm sql.NullInt64
	if err := rows.Scan(&p.Ts, &p.Site, &p.CPU, &p.Mem, &p.Procs, &p.Req, &p.Bytes, &p.R4, &p.R5, &fpm, &p.FpmReq); err != nil {
		return p, err
	}
	p.FpmActive = fpm.Int64
	return p, nil
}

// QueryMinuteSeries returns minute samples since `since`, aggregated into
// buckets of bucketSec seconds.
func (s *Store) QueryMinuteSeries(since, bucketSec int64) ([]MinutePoint, error) {
	if bucketSec < 60 {
		bucketSec = 60
	}
	rows, err := s.db.Query(`SELECT ts/? * ? AS bts, site, avg(cpu), max(mem), max(procs),
			sum(req), sum(bytes), sum(r4), sum(r5), max(fpm_active), sum(fpm_req)
		FROM samples WHERE ts >= ?
		GROUP BY bts, site ORDER BY bts`, bucketSec, bucketSec, since, bucketSec)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MinutePoint{}
	for rows.Next() {
		p, err := scanMinute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// QueryLatestMinute returns the most recent row per site.
func (s *Store) QueryLatestMinute() ([]MinutePoint, error) {
	rows, err := s.db.Query(`SELECT ` + minuteCols + ` FROM samples
		WHERE ts = (SELECT max(ts) FROM samples)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MinutePoint{}
	for rows.Next() {
		p, err := scanMinute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DiskPoint is one disk usage reading.
type DiskPoint struct {
	Ts    int64
	Site  string
	Bytes int64
}

// QueryDisk returns disk usage points since, optionally for one site.
func (s *Store) QueryDisk(since int64, site string) ([]DiskPoint, error) {
	q := `SELECT ts, site, bytes FROM disks WHERE ts >= ?`
	args := []any{since}
	if site != "" {
		q += ` AND site = ?`
		args = append(args, site)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DiskPoint{}
	for rows.Next() {
		var p DiskPoint
		if err := rows.Scan(&p.Ts, &p.Site, &p.Bytes); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// TopRow is one aggregated top-URL/IP row.
type TopRow struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
	Bytes int64  `json:"bytes"`
}

// QueryTop sums urlstats/ipstats rows for a site since a timestamp.
func (s *Store) QueryTop(table, site string, since int64, limit int) ([]TopRow, error) {
	if table != "urlstats" && table != "ipstats" {
		return nil, fmt.Errorf("bad table %q", table)
	}
	col := "path"
	if table == "ipstats" {
		col = "ip"
	}
	q := `SELECT ` + col + `, sum(cnt), sum(bytes) FROM ` + table + `
		WHERE site = ? AND win >= ? GROUP BY ` + col + ` ORDER BY 2 DESC LIMIT ?`
	rows, err := s.db.Query(q, site, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TopRow{}
	for rows.Next() {
		var r TopRow
		if err := rows.Scan(&r.Value, &r.Count, &r.Bytes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Event is one logged alert event.
type Event struct {
	Ts      int64
	Kind    string
	Site    string
	Rule    string
	Message string
}

// QueryEvents returns recent events, optionally filtered by site.
func (s *Store) QueryEvents(limit int, site string) ([]Event, error) {
	q := `SELECT ts, kind, site, rule, message FROM events`
	args := []any{}
	if site != "" {
		q += ` WHERE site = ?`
		args = append(args, site)
	}
	q += ` ORDER BY ts DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Ts, &e.Kind, &e.Site, &e.Rule, &e.Message); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
