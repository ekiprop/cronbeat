package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const (
	maxPingsPerCheck = 100
	maxBodyBytes     = 10_000
)

var ErrNotFound = errors.New("not found")

type Check struct {
	ID           string
	Name         string
	Period       time.Duration // used when Schedule is empty
	Schedule     string        // cron expression, e.g. "0 3 * * *"
	TZ           string        // IANA zone for Schedule; empty means UTC
	Grace        time.Duration
	Status       string // new, up, down
	Paused       bool
	AlertURL     string
	LastPing     time.Time
	LastStart    time.Time // set while a run is in progress
	LastDuration time.Duration
	CreatedAt    time.Time
}

// State is the status shown to humans: it adds "paused" and "late"
// (past the expected ping time but still inside the grace window).
func (c Check) State(now time.Time) string {
	switch {
	case c.Paused:
		return "paused"
	case c.Status == "up" && now.After(c.expectedAfter(c.LastPing)):
		return "late"
	case c.Status == "up" && !c.LastStart.IsZero():
		return "running"
	}
	return c.Status
}

// NextDue is when the check flips to down if no ping arrives.
func (c Check) NextDue() time.Time {
	if c.Paused {
		return time.Time{}
	}
	return c.Deadline()
}

type Ping struct {
	ID       int64
	Kind     string // start, success, fail
	ExitCode *int
	At       time.Time
	Remote   string
	Body     string
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS checks (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  period_s INTEGER NOT NULL,
  schedule TEXT NOT NULL DEFAULT '',
  tz TEXT NOT NULL DEFAULT '',
  grace_s INTEGER NOT NULL,
  status TEXT NOT NULL DEFAULT 'new',
  paused INTEGER NOT NULL DEFAULT 0,
  alert_url TEXT NOT NULL DEFAULT '',
  last_ping_ms INTEGER NOT NULL DEFAULT 0,
  last_start_ms INTEGER NOT NULL DEFAULT 0,
  last_duration_ms INTEGER NOT NULL DEFAULT 0,
  deadline_ms INTEGER NOT NULL DEFAULT 0, -- when an up check goes down; computed in Go
  created_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS pings (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  check_id TEXT NOT NULL REFERENCES checks(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  exit_code INTEGER,
  at_ms INTEGER NOT NULL,
  remote TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS pings_check ON pings(check_id, id);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

func OpenStore(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a single connection keeps things simple and serialised.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate creates the schema and adds columns that databases from older
// versions are missing.
func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	have := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM pragma_table_info('checks')`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	for _, col := range []struct{ name, ddl string }{
		{"schedule", `ALTER TABLE checks ADD COLUMN schedule TEXT NOT NULL DEFAULT ''`},
		{"tz", `ALTER TABLE checks ADD COLUMN tz TEXT NOT NULL DEFAULT ''`},
		{"deadline_ms", `ALTER TABLE checks ADD COLUMN deadline_ms INTEGER NOT NULL DEFAULT 0`},
	} {
		if have[col.name] {
			continue
		}
		if _, err := db.Exec(col.ddl); err != nil {
			return err
		}
		if col.name == "deadline_ms" {
			// Older versions only had fixed periods, so the deadline is plain arithmetic.
			if _, err := db.Exec(`UPDATE checks SET deadline_ms = last_ping_ms + (period_s + grace_s) * 1000
				WHERE status='up' AND last_ping_ms > 0`); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// APIKey returns the persisted API key, generating one on first run.
func (s *Store) APIKey() (key string, created bool, err error) {
	err = s.db.QueryRow(`SELECT value FROM settings WHERE key='api_key'`).Scan(&key)
	if err == nil {
		return key, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	key = "cb_" + randomHex(24)
	_, err = s.db.Exec(`INSERT INTO settings(key, value) VALUES('api_key', ?)`, key)
	return key, true, err
}

const checkCols = `id, name, period_s, schedule, tz, grace_s, status, paused, alert_url, last_ping_ms, last_start_ms, last_duration_ms, created_ms`

type rowScanner interface{ Scan(dest ...any) error }

func msTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func timeMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func scanCheck(r rowScanner) (Check, error) {
	var c Check
	var period, grace, lastPing, lastStart, lastDur, created int64
	err := r.Scan(&c.ID, &c.Name, &period, &c.Schedule, &c.TZ, &grace, &c.Status, &c.Paused, &c.AlertURL, &lastPing, &lastStart, &lastDur, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.Period = time.Duration(period) * time.Second
	c.Grace = time.Duration(grace) * time.Second
	c.LastPing = msTime(lastPing)
	c.LastStart = msTime(lastStart)
	c.LastDuration = time.Duration(lastDur) * time.Millisecond
	c.CreatedAt = msTime(created)
	return c, nil
}

// CreateCheck stores a new check built from c's user-editable fields.
func (s *Store) CreateCheck(c Check, now time.Time) (Check, error) {
	c.ID, c.Status, c.CreatedAt = newUUID(), "new", now
	_, err := s.db.Exec(`INSERT INTO checks(id, name, period_s, schedule, tz, grace_s, alert_url, paused, created_ms) VALUES(?,?,?,?,?,?,?,?,?)`,
		c.ID, c.Name, int64(c.Period/time.Second), c.Schedule, c.TZ, int64(c.Grace/time.Second), c.AlertURL, c.Paused, now.UnixMilli())
	return c, err
}

func (s *Store) GetCheck(id string) (Check, error) {
	return scanCheck(s.db.QueryRow(`SELECT `+checkCols+` FROM checks WHERE id=?`, id))
}

func (s *Store) ListChecks() ([]Check, error) {
	rows, err := s.db.Query(`SELECT ` + checkCols + ` FROM checks ORDER BY name, created_ms`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Check
	for rows.Next() {
		c, err := scanCheck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateCheck saves the user-editable fields and recomputes the deadline,
// since a schedule change moves it.
func (s *Store) UpdateCheck(c Check) error {
	res, err := s.db.Exec(`UPDATE checks SET name=?, period_s=?, schedule=?, tz=?, grace_s=?, alert_url=?, paused=?, deadline_ms=? WHERE id=?`,
		c.Name, int64(c.Period/time.Second), c.Schedule, c.TZ, int64(c.Grace/time.Second), c.AlertURL, c.Paused, timeMs(c.Deadline()), c.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteCheck(id string) error {
	res, err := s.db.Exec(`DELETE FROM checks WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordPing applies a ping to a check and returns the status before the ping
// along with the updated check, so the caller can alert on transitions.
func (s *Store) RecordPing(id, kind string, exitCode *int, remote, body string, now time.Time) (prev string, c Check, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", c, err
	}
	defer tx.Rollback()

	c, err = scanCheck(tx.QueryRow(`SELECT `+checkCols+` FROM checks WHERE id=?`, id))
	if err != nil {
		return "", c, err
	}
	prev = c.Status
	switch kind {
	case "start":
		c.LastStart = now
	case "success", "fail":
		if !c.LastStart.IsZero() {
			c.LastDuration = now.Sub(c.LastStart)
			c.LastStart = time.Time{}
		}
		c.LastPing = now
		c.Status = "up"
		if kind == "fail" {
			c.Status = "down"
		}
	default:
		return "", c, fmt.Errorf("unknown ping kind %q", kind)
	}
	// Any ping means the job is alive again, so it un-pauses the check.
	c.Paused = false

	if _, err = tx.Exec(`UPDATE checks SET status=?, paused=0, last_ping_ms=?, last_start_ms=?, last_duration_ms=?, deadline_ms=? WHERE id=?`,
		c.Status, timeMs(c.LastPing), timeMs(c.LastStart), c.LastDuration.Milliseconds(), timeMs(c.Deadline()), id); err != nil {
		return "", c, err
	}
	if len(body) > maxBodyBytes {
		body = body[len(body)-maxBodyBytes:]
	}
	if _, err = tx.Exec(`INSERT INTO pings(check_id, kind, exit_code, at_ms, remote, body) VALUES(?,?,?,?,?,?)`,
		id, kind, exitCode, now.UnixMilli(), remote, body); err != nil {
		return "", c, err
	}
	if _, err = tx.Exec(`DELETE FROM pings WHERE check_id=? AND id NOT IN
		(SELECT id FROM pings WHERE check_id=? ORDER BY id DESC LIMIT ?)`, id, id, maxPingsPerCheck); err != nil {
		return "", c, err
	}
	return prev, c, tx.Commit()
}

func (s *Store) Pings(id string, limit int) ([]Ping, error) {
	rows, err := s.db.Query(`SELECT id, kind, exit_code, at_ms, remote, body FROM pings WHERE check_id=? ORDER BY id DESC LIMIT ?`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ping
	for rows.Next() {
		var p Ping
		var code sql.NullInt64
		var at int64
		if err := rows.Scan(&p.ID, &p.Kind, &code, &at, &p.Remote, &p.Body); err != nil {
			return nil, err
		}
		if code.Valid {
			v := int(code.Int64)
			p.ExitCode = &v
		}
		p.At = msTime(at)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Overdue condition, shared by the SELECT and the guarded UPDATE so a ping that
// lands between the two can't be clobbered. Params: now, now.
const overdueCond = `paused=0 AND status!='down' AND (
  (status='up' AND deadline_ms > 0 AND deadline_ms < ?) OR
  (last_start_ms > 0 AND last_start_ms + grace_s * 1000 < ?))`

type Overdue struct {
	Check  Check
	Reason string
}

// MarkOverdue flips checks that missed their deadline to down and returns them.
func (s *Store) MarkOverdue(now time.Time) ([]Overdue, error) {
	ms := now.UnixMilli()
	rows, err := s.db.Query(`SELECT `+checkCols+` FROM checks WHERE `+overdueCond, ms, ms)
	if err != nil {
		return nil, err
	}
	var candidates []Check
	for rows.Next() {
		c, err := scanCheck(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []Overdue
	for _, c := range candidates {
		res, err := s.db.Exec(`UPDATE checks SET status='down' WHERE id=? AND `+overdueCond, c.ID, ms, ms)
		if err != nil {
			return out, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		var reason string
		if !c.LastStart.IsZero() && c.LastStart.Add(c.Grace).Before(now) {
			reason = fmt.Sprintf("started %s ago but never finished (grace %s)", fmtDur(now.Sub(c.LastStart)), fmtDur(c.Grace))
		} else {
			reason = fmt.Sprintf("no ping for %s (schedule %s, grace %s)", fmtDur(now.Sub(c.LastPing)), c.Describe(), fmtDur(c.Grace))
		}
		c.Status = "down"
		out = append(out, Overdue{Check: c, Reason: reason})
	}
	return out, nil
}
