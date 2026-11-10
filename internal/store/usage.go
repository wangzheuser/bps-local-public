package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// UsageRequest is one finished bridge request with its outcome and token usage.
// Token fields are NULL when the upstream did not report them.
type UsageRequest struct {
	ID              string
	APIKeyID        *string
	AccountID       *string
	RequestedModel  string
	ReportedModel   string
	ReasoningEffort string
	Status          string
	HTTPStatus      int
	UpstreamStatus  int
	TerminalEvent   string
	ErrorCode       string
	InputTokens     sql.NullInt64
	OutputTokens    sql.NullInt64
	TotalTokens     sql.NullInt64
	CachedTokens    sql.NullInt64
	ReasoningTokens sql.NullInt64
	StartedAt       int64
	FinishedAt      int64
	DurationMS      int64
}

// InsertUsageRequest persists one finished request.
func (d *DB) InsertUsageRequest(u *UsageRequest) error {
	_, err := d.Exec(`INSERT INTO usage_requests (
		id, api_key_id, account_id, requested_model, reported_model, reasoning_effort,
		status, http_status, upstream_status, terminal_event, error_code,
		input_tokens, output_tokens, total_tokens, cached_tokens, reasoning_tokens,
		started_at, finished_at, duration_ms
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.APIKeyID, u.AccountID, u.RequestedModel, u.ReportedModel, u.ReasoningEffort,
		u.Status, u.HTTPStatus, u.UpstreamStatus, u.TerminalEvent, u.ErrorCode,
		u.InputTokens, u.OutputTokens, u.TotalTokens, u.CachedTokens, u.ReasoningTokens,
		u.StartedAt, u.FinishedAt, u.DurationMS)
	if err != nil {
		return fmt.Errorf("insert usage request: %w", err)
	}
	return nil
}

// UsageFilters narrows usage queries. Zero values are ignored.
type UsageFilters struct {
	From     int64 // Unix ms
	To       int64 // Unix ms
	AccountID string
	APIKeyID  string
	Model     string
	Limit     int
	Offset    int
}

func usageWhere(f UsageFilters) (string, []any) {
	var clauses []string
	var args []any
	if f.From > 0 {
		clauses = append(clauses, `started_at >= ?`)
		args = append(args, f.From)
	}
	if f.To > 0 {
		clauses = append(clauses, `started_at <= ?`)
		args = append(args, f.To)
	}
	if f.AccountID != "" {
		clauses = append(clauses, `account_id = ?`)
		args = append(args, f.AccountID)
	}
	if f.APIKeyID != "" {
		clauses = append(clauses, `api_key_id = ?`)
		args = append(args, f.APIKeyID)
	}
	if f.Model != "" {
		clauses = append(clauses, `requested_model = ?`)
		args = append(args, f.Model)
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// ListUsageRequests returns finished requests newest first with a bounded page.
func (d *DB) ListUsageRequests(f UsageFilters) ([]*UsageRequest, int, error) {
	where, args := usageWhere(f)
	var total int
	if err := d.QueryRow(`SELECT COUNT(*) FROM usage_requests`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	query := `SELECT id, api_key_id, account_id, requested_model, reported_model, reasoning_effort,
		status, http_status, upstream_status, terminal_event, error_code,
		input_tokens, output_tokens, total_tokens, cached_tokens, reasoning_tokens,
		started_at, finished_at, duration_ms
		FROM usage_requests` + where + ` ORDER BY started_at DESC, id DESC LIMIT ? OFFSET ?`
	rows, err := d.Query(query, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*UsageRequest
	for rows.Next() {
		u := &UsageRequest{}
		var keyID, acctID sql.NullString
		if err := rows.Scan(&u.ID, &keyID, &acctID, &u.RequestedModel, &u.ReportedModel, &u.ReasoningEffort,
			&u.Status, &u.HTTPStatus, &u.UpstreamStatus, &u.TerminalEvent, &u.ErrorCode,
			&u.InputTokens, &u.OutputTokens, &u.TotalTokens, &u.CachedTokens, &u.ReasoningTokens,
			&u.StartedAt, &u.FinishedAt, &u.DurationMS); err != nil {
			return nil, 0, err
		}
		u.APIKeyID = nullStringPtr(keyID)
		u.AccountID = nullStringPtr(acctID)
		out = append(out, u)
	}
	return out, total, rows.Err()
}

// SummaryRow is one aggregate bucket for the usage summary endpoint.
type SummaryRow struct {
	Label        string
	Requests     int64
	Completed    int64
	Incomplete   int64
	Failed       int64
	InputTokens  sql.NullInt64
	OutputTokens sql.NullInt64
	TotalTokens  sql.NullInt64
	AvgDuration  sql.NullFloat64
}

// UsageSummary aggregates usage grouped by day, account, key or model.
func (d *DB) UsageSummary(f UsageFilters, groupBy string) ([]*SummaryRow, error) {
	var label string
	switch groupBy {
	case "day":
		label = `strftime('%Y-%m-%d', started_at/1000, 'unixepoch')`
	case "account":
		label = `COALESCE(NULLIF(account_id, ''), '(none)')`
	case "api_key":
		label = `COALESCE(NULLIF(api_key_id, ''), '(none)')`
	case "model":
		label = `COALESCE(NULLIF(requested_model, ''), '(none)')`
	default:
		return nil, fmt.Errorf("unsupported group_by %q", groupBy)
	}
	where, args := usageWhere(f)
	query := `SELECT ` + label + ` AS label,
		COUNT(*) AS requests,
		SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END) AS completed,
		SUM(CASE WHEN status = 'incomplete' THEN 1 ELSE 0 END) AS incomplete,
		SUM(CASE WHEN status IN ('failed', 'http_error', 'upstream_eof') THEN 1 ELSE 0 END) AS failed,
		SUM(input_tokens), SUM(output_tokens), SUM(total_tokens), AVG(duration_ms)
		FROM usage_requests` + where + ` GROUP BY label ORDER BY label DESC`
	rows, err := d.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SummaryRow
	for rows.Next() {
		r := &SummaryRow{}
		if err := rows.Scan(&r.Label, &r.Requests, &r.Completed, &r.Incomplete, &r.Failed,
			&r.InputTokens, &r.OutputTokens, &r.TotalTokens, &r.AvgDuration); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageOverview holds today's headline numbers for the dashboard.
type UsageOverview struct {
	Requests      int64
	Completed     int64
	Failed        int64
	InputTokens   sql.NullInt64
	OutputTokens  sql.NullInt64
	TotalTokens   sql.NullInt64
	AvgDurationMS sql.NullFloat64
}

// UsageOverviewToday returns aggregates since the start of the UTC day.
// Empty tables aggregate to zero instead of NULL so scans stay simple.
func (d *DB) UsageOverviewToday(dayStartMS int64) (*UsageOverview, error) {
	o := &UsageOverview{}
	err := d.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status IN ('failed', 'http_error', 'upstream_eof') THEN 1 ELSE 0 END), 0),
		SUM(input_tokens), SUM(output_tokens), SUM(total_tokens), AVG(duration_ms)
		FROM usage_requests WHERE started_at >= ?`, dayStartMS).Scan(
		&o.Requests, &o.Completed, &o.Failed, &o.InputTokens, &o.OutputTokens, &o.TotalTokens, &o.AvgDurationMS)
	if err != nil {
		return nil, err
	}
	return o, nil
}

// DeleteUsageBefore removes raw request records older than the cutoff and
// returns the number of deleted rows.
func (d *DB) DeleteUsageBefore(cutoffMS int64) (int64, error) {
	res, err := d.Exec(`DELETE FROM usage_requests WHERE started_at < ?`, cutoffMS)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func nullStringPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}
