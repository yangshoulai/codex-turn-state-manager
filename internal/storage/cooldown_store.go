package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/yangshoulai/codex-turn-state-manager/internal/proxies"
)

// The per-(account, proxy) failure ledger.
//
// These are methods on ProxyStore rather than a type of their own because
// proxies.Pool takes one Store, and splitting the pool's state across two
// objects would only move the seam.

// ListCooldowns implements proxies.Store.
func (s *ProxyStore) ListCooldowns(ctx context.Context) ([]proxies.Cooldown, error) {
	rows, err := s.db.sql.QueryContext(ctx, `
		SELECT auth_index, proxy_id, consecutive_failures, failure_count,
		       COALESCE(cooldown_until, ''), COALESCE(last_failure, '')
		FROM proxy_cooldown
		ORDER BY auth_index, proxy_id`)
	if err != nil {
		return nil, fmt.Errorf("storage: list cooldowns: %w", err)
	}
	defer rows.Close()

	var out []proxies.Cooldown
	for rows.Next() {
		var (
			c                  proxies.Cooldown
			cooldown, lastFail string
		)
		if err := rows.Scan(&c.AuthIndex, &c.ProxyID, &c.ConsecutiveFailures,
			&c.FailureCount, &cooldown, &lastFail); err != nil {
			return nil, fmt.Errorf("storage: scan cooldown: %w", err)
		}
		c.CooldownUntil = timePtr(ParseTime(cooldown))
		c.LastFailure = timePtr(ParseTime(lastFail))
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpsertCooldown implements proxies.Store.
func (s *ProxyStore) UpsertCooldown(ctx context.Context, c proxies.Cooldown) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO proxy_cooldown
				(auth_index, proxy_id, consecutive_failures, failure_count,
				 cooldown_until, last_failure)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(auth_index, proxy_id) DO UPDATE SET
				consecutive_failures = excluded.consecutive_failures,
				failure_count        = excluded.failure_count,
				cooldown_until       = excluded.cooldown_until,
				last_failure         = excluded.last_failure`,
			c.AuthIndex, c.ProxyID, c.ConsecutiveFailures, c.FailureCount,
			FormatTimePtr(c.CooldownUntil), FormatTimePtr(c.LastFailure))
		if err != nil {
			return fmt.Errorf("storage: upsert cooldown %s/%s: %w", c.AuthIndex, c.ProxyID, err)
		}
		return nil
	})
}

// DeleteCooldown implements proxies.Store.
func (s *ProxyStore) DeleteCooldown(ctx context.Context, authIndex, proxyID string) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM proxy_cooldown WHERE auth_index = ? AND proxy_id = ?`, authIndex, proxyID)
		return err
	})
}

// DeleteAllCooldowns implements proxies.Store.
func (s *ProxyStore) DeleteAllCooldowns(ctx context.Context) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM proxy_cooldown`)
		return err
	})
}

// DeleteCooldownsForProxy drops every account's record against one node, used
// when the node itself is removed so the ledger does not outlive its subject.
func (s *ProxyStore) DeleteCooldownsForProxy(ctx context.Context, proxyID string) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM proxy_cooldown WHERE proxy_id = ?`, proxyID)
		return err
	})
}
