package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Setting keys persisted in the settings table.
const (
	SettingProxyURL = "proxy_url"
)

// GetSetting returns one setting value. A missing key yields ErrNotFound so
// callers can distinguish "never configured" from "configured as empty".
func (d *DB) GetSetting(key string) (string, error) {
	var value string
	err := d.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read setting %s: %w", key, err)
	}
	return value, nil
}

// SetSetting upserts one setting value.
func (d *DB) SetSetting(key, value string) error {
	_, err := d.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("write setting %s: %w", key, err)
	}
	return nil
}
