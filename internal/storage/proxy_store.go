package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/proxies"
)

// ProxyStore implements proxies.Store against SQLite.
type ProxyStore struct{ db *DB }

// Proxies returns the proxy store.
func (db *DB) Proxies() *ProxyStore { return &ProxyStore{db: db} }

// ListProxies implements proxies.Store.
func (s *ProxyStore) ListProxies(ctx context.Context) ([]proxies.Node, error) {
	rows, err := s.db.sql.QueryContext(ctx, `
		SELECT id, url, enabled, success_count, failure_count, consecutive_failures,
		       COALESCE(cooldown_until, ''), last_latency_ms,
		       COALESCE(last_used_at, ''), COALESCE(last_success, ''), COALESCE(last_failure, '')
		FROM proxy_node
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("storage: list proxies: %w", err)
	}
	defer rows.Close()

	var out []proxies.Node
	for rows.Next() {
		var (
			n                                    proxies.Node
			enabled                              int
			cooldown, lastUsed, lastOK, lastFail string
			latency                              sql.NullInt64
		)
		if err := rows.Scan(&n.ID, &n.URL, &enabled, &n.SuccessCount, &n.FailureCount,
			&n.ConsecutiveFailures, &cooldown, &latency, &lastUsed, &lastOK, &lastFail); err != nil {
			return nil, fmt.Errorf("storage: scan proxy: %w", err)
		}
		n.Enabled = enabled != 0
		n.CooldownUntil = timePtr(ParseTime(cooldown))
		n.LastUsedAt = timePtr(ParseTime(lastUsed))
		n.LastSuccess = timePtr(ParseTime(lastOK))
		n.LastFailure = timePtr(ParseTime(lastFail))
		if latency.Valid {
			ms := int(latency.Int64)
			n.LastLatencyMS = &ms
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// UpsertProxy implements proxies.Store. The update is a single statement so two
// probe workers touching last_used_at concurrently cannot interleave a
// read-modify-write (design doc 6.2).
func (s *ProxyStore) UpsertProxy(ctx context.Context, n proxies.Node) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO proxy_node
				(id, url, enabled, success_count, failure_count, consecutive_failures,
				 cooldown_until, last_latency_ms, last_used_at, last_success, last_failure)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				url                  = excluded.url,
				enabled              = excluded.enabled,
				success_count        = excluded.success_count,
				failure_count        = excluded.failure_count,
				consecutive_failures = excluded.consecutive_failures,
				cooldown_until       = excluded.cooldown_until,
				last_latency_ms      = excluded.last_latency_ms,
				last_used_at         = excluded.last_used_at,
				last_success         = excluded.last_success,
				last_failure         = excluded.last_failure`,
			n.ID, n.URL, boolInt(n.Enabled), n.SuccessCount, n.FailureCount,
			n.ConsecutiveFailures, FormatTimePtr(n.CooldownUntil), intPtr(n.LastLatencyMS),
			FormatTimePtr(n.LastUsedAt), FormatTimePtr(n.LastSuccess), FormatTimePtr(n.LastFailure))
		if err != nil {
			return fmt.Errorf("storage: upsert proxy %s: %w", n.ID, err)
		}
		return nil
	})
}

// DeleteProxy implements proxies.Store.
func (s *ProxyStore) DeleteProxy(ctx context.Context, id string) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM proxy_node WHERE id = ?`, id)
		return err
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func intPtr(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

var _ proxies.Store = (*ProxyStore)(nil)
