// Package store persists account metadata, API key hashes and usage statistics
// in SQLite. Credential tokens stay in OAuth files and never enter this store.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// DB wraps the SQLite handle with the pragmas this service needs.
type DB struct {
	*sql.DB
}

// Open creates the parent directory and opens the database with WAL mode,
// foreign keys and a busy timeout. A single connection serializes access so
// writers never race inside one process.
func Open(path string) (*DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("empty database path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	dsn := "file:" + strings.ReplaceAll(filepath.ToSlash(path), "\\", "/") +
		"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return &DB{DB: db}, nil
}

// Close releases the database handle.
func (d *DB) Close() error { return d.DB.Close() }
