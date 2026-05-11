package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GetPluginCapabilities returns the cached --init document for `name`.
// The bool reports whether a row was found; (zero-value, false, nil) is
// the cache-miss signal. CapabilitiesJSON is opaque to the store —
// callers parse it back into plugins.Capabilities at the boundary.
func (s *sqliteStore) GetPluginCapabilities(ctx context.Context, name string) (CachedPluginCapabilities, bool, error) {
	var (
		entry CachedPluginCapabilities
		cachedAtTS string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT version, capabilities_json, cached_at FROM plugin_capabilities WHERE plugin_name = ?`,
		name,
	).Scan(&entry.Version, &entry.CapabilitiesJSON, &cachedAtTS)
	if errors.Is(err, sql.ErrNoRows) {
		return CachedPluginCapabilities{}, false, nil
	}
	if err != nil {
		return CachedPluginCapabilities{}, false, fmt.Errorf("get plugin_capabilities row: %w", err)
	}
	t, err := time.Parse(time.RFC3339Nano, cachedAtTS)
	if err != nil {
		// Fall back to RFC3339 without nanos — mirrors what the
		// provenance reader does for SQLite-roundtripped timestamps.
		t, err = time.Parse(time.RFC3339, cachedAtTS)
		if err != nil {
			return CachedPluginCapabilities{}, false, fmt.Errorf("parse cached_at %q: %w", cachedAtTS, err)
		}
	}
	entry.CachedAt = t
	return entry, true, nil
}

// UpsertPluginCapabilities writes the cache row for `name`, overwriting
// in place when one exists. cached_at is stamped from the wall clock at
// call time (UTC, RFC3339Nano).
func (s *sqliteStore) UpsertPluginCapabilities(ctx context.Context, name, version string, capabilitiesJSON []byte) error {
	if name == "" {
		return errors.New("UpsertPluginCapabilities: empty name")
	}
	if version == "" {
		return errors.New("UpsertPluginCapabilities: empty version")
	}
	if len(capabilitiesJSON) == 0 {
		return errors.New("UpsertPluginCapabilities: empty capabilities_json")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO plugin_capabilities (plugin_name, version, capabilities_json, cached_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(plugin_name) DO UPDATE SET
		 version = excluded.version,
		 capabilities_json = excluded.capabilities_json,
		 cached_at = excluded.cached_at`,
		name, version, capabilitiesJSON, now)
	if err != nil {
		return fmt.Errorf("upsert plugin_capabilities: %w", err)
	}
	return nil
}

// DeletePluginCapabilities drops the cache row for `name`. Returns
// (true, nil) when a row was actually deleted, (false, nil) when no
// row matched. Used by `alice2-index plugins clear-cache --name <n>`.
func (s *sqliteStore) DeletePluginCapabilities(ctx context.Context, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM plugin_capabilities WHERE plugin_name = ?`,
		name)
	if err != nil {
		return false, fmt.Errorf("delete plugin_capabilities: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete plugin_capabilities rows-affected: %w", err)
	}
	return rows > 0, nil
}

// ClearAllPluginCapabilities truncates the plugin_capabilities table
// and returns the count of rows deleted. Used by
// `alice2-index plugins clear-cache` (no --name flag).
func (s *sqliteStore) ClearAllPluginCapabilities(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM plugin_capabilities`)
	if err != nil {
		return 0, fmt.Errorf("clear plugin_capabilities: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("clear plugin_capabilities rows-affected: %w", err)
	}
	return int(rows), nil
}
