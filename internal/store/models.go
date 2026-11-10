package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ModelCatalog is the persisted snapshot of the upstream model list.
type ModelCatalog struct {
	Payload       []byte
	SyncedAt      int64
	SourceAccount string
}

// ModelMapping aliases one requested model onto another.
type ModelMapping struct {
	SourceModel string
	TargetModel string
	CreatedAt   int64
	UpdatedAt   int64
}

// modelCatalogID is the single snapshot row. Accounts currently share one
// catalog, so a fixed id keeps the table self-describing.
const modelCatalogID = "default"

// SaveModelCatalog replaces the snapshot. The payload is stored verbatim so
// /v1/models can return exactly what the upstream reported.
func (d *DB) SaveModelCatalog(payload []byte, sourceAccount string) error {
	if len(payload) == 0 {
		return fmt.Errorf("model catalog payload is empty")
	}
	_, err := d.Exec(`INSERT INTO model_catalog (id, payload, synced_at, source_account)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			payload = excluded.payload,
			synced_at = excluded.synced_at,
			source_account = excluded.source_account`,
		modelCatalogID, string(payload), time.Now().UnixMilli(), sourceAccount)
	if err != nil {
		return fmt.Errorf("save model catalog: %w", err)
	}
	return nil
}

// GetModelCatalog returns the last synced snapshot. ErrNotFound means the
// console has never synced, which callers surface as an actionable error
// rather than an empty model list.
func (d *DB) GetModelCatalog() (*ModelCatalog, error) {
	var payload, source string
	var syncedAt int64
	err := d.QueryRow(`SELECT payload, synced_at, source_account FROM model_catalog WHERE id = ?`,
		modelCatalogID).Scan(&payload, &syncedAt, &source)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read model catalog: %w", err)
	}
	return &ModelCatalog{Payload: []byte(payload), SyncedAt: syncedAt, SourceAccount: source}, nil
}

// ListModelMappings returns every mapping ordered by source model.
func (d *DB) ListModelMappings() ([]ModelMapping, error) {
	rows, err := d.Query(`SELECT source_model, target_model, created_at, updated_at
		FROM model_mappings ORDER BY source_model`)
	if err != nil {
		return nil, fmt.Errorf("list model mappings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]ModelMapping, 0, 16)
	for rows.Next() {
		var m ModelMapping
		if err := rows.Scan(&m.SourceModel, &m.TargetModel, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan model mapping: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model mappings: %w", err)
	}
	return out, nil
}

// ReplaceModelMappings swaps the whole mapping set in one transaction. The
// console edits the list as a unit, so a partial apply must never happen.
func (d *DB) ReplaceModelMappings(pairs []ModelMapping) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("begin model mapping replace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM model_mappings`); err != nil {
		return fmt.Errorf("clear model mappings: %w", err)
	}
	now := time.Now().UnixMilli()
	for _, m := range pairs {
		if _, err := tx.Exec(`INSERT INTO model_mappings
			(source_model, target_model, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			m.SourceModel, m.TargetModel, now, now); err != nil {
			return fmt.Errorf("insert model mapping %s: %w", m.SourceModel, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit model mapping replace: %w", err)
	}
	return nil
}

// GetModelMapping resolves one source model. ErrNotFound means the model is
// passed through unchanged.
func (d *DB) GetModelMapping(source string) (string, error) {
	var target string
	err := d.QueryRow(`SELECT target_model FROM model_mappings WHERE source_model = ?`, source).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read model mapping %s: %w", source, err)
	}
	return target, nil
}
