package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const (
	KindCPU = "cpu"
	KindGPU = "gpu"
)

// Hardware represents a benchmarked CPU or GPU entry in hardware.db.
type Hardware struct {
	ID              string  `json:"id"`
	Kind            string  `json:"kind"`
	Name            string  `json:"name"`
	NormalizedName  string  `json:"normalized_name"`
	Vendor          string  `json:"vendor"`
	MultiScore      float64 `json:"multi_score"`
	SingleScore     float64 `json:"single_score"`
	GamingRankScore float64 `json:"gaming_rank_score"`
	PassMarkID      string  `json:"passmark_id,omitempty"`
}

// Metadata holds build-time information stored in the metadata table.
type Metadata struct {
	SchemaVersion        string `json:"schema_version"`
	GeneratedAt          string `json:"generated_at"`
	Source               string `json:"source"`
	CPUCount             int    `json:"cpu_count"`
	GPUCount             int    `json:"gpu_count"`
	CPUSingleThreadCount int    `json:"cpu_single_thread_count"`
}

// SearchItem represents a lightweight hardware autocomplete result.
type SearchItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Vendor string `json:"vendor"`
}

// SchemaDDL defines the SQLite schema for hardware.db.
const SchemaDDL = `
CREATE TABLE IF NOT EXISTS metadata (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS hardware (
    id                TEXT PRIMARY KEY,
    kind              TEXT NOT NULL CHECK (kind IN ('cpu', 'gpu')),
    name              TEXT NOT NULL,
    normalized_name   TEXT NOT NULL,
    vendor            TEXT NOT NULL,
    multi_score       REAL NOT NULL CHECK (multi_score > 0),
    single_score      REAL NOT NULL DEFAULT 0 CHECK (single_score >= 0),
    gaming_rank_score REAL NOT NULL CHECK (gaming_rank_score > 0),
    passmark_id       TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_hardware_kind_norm
    ON hardware (kind, normalized_name);

CREATE INDEX IF NOT EXISTS idx_hardware_kind_rank
    ON hardware (kind, gaming_rank_score DESC);

CREATE VIRTUAL TABLE IF NOT EXISTS hardware_fts USING fts5(
    id UNINDEXED,
    kind UNINDEXED,
    name,
    normalized_name,
    tokenize = 'unicode61 remove_diacritics 2'
);
`

// OpenReadOnly opens hardware.db in strict read-only immutable mode suitable
// for FROM scratch / --read-only containers without temp disk writes.
func OpenReadOnly(dbPath string) (*sql.DB, error) {
	slashPath := filepath.ToSlash(dbPath)
	dsn := fmt.Sprintf("file:%s?mode=ro&immutable=1&_pragma=temp_store(MEMORY)&_pragma=query_only(ON)", slashPath)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open read-only sqlite %q: %w", dbPath, err)
	}
	conn.SetMaxOpenConns(4)
	conn.SetMaxIdleConns(4)
	if err := conn.Ping(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping read-only sqlite %q: %w", dbPath, err)
	}
	return conn, nil
}

// OpenReadWrite opens hardware.db for sync/build operations.
func OpenReadWrite(dbPath string) (*sql.DB, error) {
	slashPath := filepath.ToSlash(dbPath)
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)", slashPath)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open read-write sqlite %q: %w", dbPath, err)
	}
	conn.SetMaxOpenConns(1)
	if err := conn.Ping(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping read-write sqlite %q: %w", dbPath, err)
	}
	return conn, nil
}

// InitSchema applies the hardware.db DDL on a writable connection.
func InitSchema(ctx context.Context, conn *sql.DB) error {
	if _, err := conn.ExecContext(ctx, SchemaDDL); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	return nil
}
