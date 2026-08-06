// Package db owns the SQLite file. It is the source of truth; everything else
// is a generated view.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// DefaultPath resolves the DB location the way the Python build did: the
// TRACKER_DB override, else tracker.db next to the executable's project root.
func DefaultPath() string {
	if p := os.Getenv("TRACKER_DB"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		if root, err := filepath.Abs(filepath.Dir(exe)); err == nil {
			return filepath.Join(root, "tracker.db")
		}
	}
	return "tracker.db"
}

// ISO8601 is the timestamp format stored in every *_at column: UTC, second
// precision. It matches datetime.now(timezone.utc).isoformat(timespec="seconds").
//
// The offset must be spelled "-07:00", not "Z07:00": Go renders UTC as a bare
// "Z" under the latter, while Python emits "+00:00". Rows written in the two
// formats no longer sort against each other, and every follow-up query compares
// these timestamps as strings.
const ISO8601 = "2006-01-02T15:04:05-07:00"

func Now() string {
	return time.Now().UTC().Format(ISO8601)
}

// Connect opens the database, applies the schema, and migrates in place.
func Connect(path string) (*sql.DB, error) {
	if path == "" {
		path = DefaultPath()
	}
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// SQLite tolerates exactly one writer; the pollers run concurrently, so
	// serialising here is cheaper than handling SQLITE_BUSY at every call site.
	conn.SetMaxOpenConns(1)

	if _, err := conn.Exec("PRAGMA foreign_keys = ON"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}
	if _, err := conn.Exec(Schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := Migrate(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// Migrate adds any column this version expects but the file on disk lacks.
func Migrate(conn *sql.DB) error {
	for _, spec := range addedColumns {
		have, err := columnNames(conn, spec.Table)
		if err != nil {
			return err
		}
		for _, col := range spec.Columns {
			if _, ok := have[col.Name]; ok {
				continue
			}
			// Table and column names are compile-time constants from
			// addedColumns, never user input, so interpolation is safe here —
			// ALTER TABLE takes no placeholders.
			stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", spec.Table, col.Name, col.Decl)
			if _, err := conn.Exec(stmt); err != nil {
				return fmt.Errorf("migrate %s.%s: %w", spec.Table, col.Name, err)
			}
		}
	}
	for _, stmt := range postIndexes {
		if _, err := conn.Exec(stmt); err != nil {
			return fmt.Errorf("create index: %w", err)
		}
	}
	return nil
}

func columnNames(conn *sql.DB, table string) (map[string]struct{}, error) {
	rows, err := conn.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, fmt.Errorf("table_info %s: %w", table, err)
	}
	defer rows.Close()

	have := make(map[string]struct{})
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultVal, &pk); err != nil {
			return nil, fmt.Errorf("scan table_info %s: %w", table, err)
		}
		have[name] = struct{}{}
	}
	return have, rows.Err()
}

// LogExclusion records every exclusion as a greppable row. Nothing is dropped
// silently.
func LogExclusion(conn *sql.DB, rawPayload, reason, ruleID string) error {
	_, err := conn.Exec(
		"INSERT INTO excluded_log (raw_payload, reason, rule_id, logged_at) VALUES (?,?,?,?)",
		rawPayload, reason, ruleID, Now(),
	)
	if err != nil {
		return fmt.Errorf("log exclusion %s: %w", ruleID, err)
	}
	return nil
}
