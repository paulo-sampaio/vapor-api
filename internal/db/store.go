package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Store provides read-only queries against hardware.db.
type Store struct {
	db *sql.DB
}

// NewStore creates a Store backed by an opened SQLite connection.
func NewStore(conn *sql.DB) *Store {
	return &Store{db: conn}
}

// LoadAll returns every hardware row in the catalog.
func (s *Store) LoadAll(ctx context.Context) ([]Hardware, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, name, normalized_name, vendor, multi_score, single_score, gaming_rank_score, passmark_id
		FROM hardware
		ORDER BY kind, gaming_rank_score DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("load all hardware: %w", err)
	}
	defer rows.Close()

	var items []Hardware
	for rows.Next() {
		var h Hardware
		if err := rows.Scan(
			&h.ID, &h.Kind, &h.Name, &h.NormalizedName,
			&h.Vendor, &h.MultiScore, &h.SingleScore, &h.GamingRankScore, &h.PassMarkID,
		); err != nil {
			return nil, fmt.Errorf("scan hardware row: %w", err)
		}
		items = append(items, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate hardware rows: %w", err)
	}
	return items, nil
}

// GetMetadata reads build metadata from the metadata table.
func (s *Store) GetMetadata(ctx context.Context) (Metadata, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM metadata`)
	if err != nil {
		return Metadata{}, fmt.Errorf("query metadata: %w", err)
	}
	defer rows.Close()

	var meta Metadata
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return Metadata{}, fmt.Errorf("scan metadata: %w", err)
		}
		switch k {
		case "schema_version":
			meta.SchemaVersion = v
		case "generated_at":
			meta.GeneratedAt = v
		case "source":
			meta.Source = v
		case "cpu_count":
			meta.CPUCount, _ = strconv.Atoi(v)
		case "gpu_count":
			meta.GPUCount, _ = strconv.Atoi(v)
		case "cpu_single_thread_count":
			meta.CPUSingleThreadCount, _ = strconv.Atoi(v)
		}
	}
	if err := rows.Err(); err != nil {
		return Metadata{}, fmt.Errorf("iterate metadata: %w", err)
	}
	return meta, nil
}

// GetByID fetches a single hardware row by its deterministic slug ID.
func (s *Store) GetByID(ctx context.Context, id string) (Hardware, bool, error) {
	var h Hardware
	err := s.db.QueryRowContext(ctx, `
		SELECT id, kind, name, normalized_name, vendor, multi_score, single_score, gaming_rank_score, passmark_id
		FROM hardware
		WHERE id = ?
	`, id).Scan(
		&h.ID, &h.Kind, &h.Name, &h.NormalizedName,
		&h.Vendor, &h.MultiScore, &h.SingleScore, &h.GamingRankScore, &h.PassMarkID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Hardware{}, false, nil
	}
	if err != nil {
		return Hardware{}, false, fmt.Errorf("get hardware by id %q: %w", id, err)
	}
	return h, true, nil
}

// ResolveUserHardware looks up a user's saved hardware by ID first, falling back
// to exact (kind, normalized_name) match if a weekly DB rebuild renamed a slug.
func (s *Store) ResolveUserHardware(ctx context.Context, kind, id, fallbackNormName string) (Hardware, bool, error) {
	if id != "" {
		h, ok, err := s.GetByID(ctx, id)
		if err != nil {
			return Hardware{}, false, err
		}
		if ok && h.Kind == kind {
			return h, true, nil
		}
	}
	if fallbackNormName == "" {
		return Hardware{}, false, nil
	}

	var h Hardware
	err := s.db.QueryRowContext(ctx, `
		SELECT id, kind, name, normalized_name, vendor, multi_score, single_score, gaming_rank_score, passmark_id
		FROM hardware
		WHERE kind = ? AND normalized_name = ?
		ORDER BY gaming_rank_score DESC
		LIMIT 1
	`, kind, fallbackNormName).Scan(
		&h.ID, &h.Kind, &h.Name, &h.NormalizedName,
		&h.Vendor, &h.MultiScore, &h.SingleScore, &h.GamingRankScore, &h.PassMarkID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Hardware{}, false, nil
	}
	if err != nil {
		return Hardware{}, false, fmt.Errorf("resolve hardware fallback (%s, %q): %w", kind, fallbackNormName, err)
	}
	return h, true, nil
}

// Search performs FTS5 prefix autocomplete search with LIKE substring fallback.
func (s *Store) Search(ctx context.Context, kind, query string, limit int) ([]SearchItem, error) {
	if limit <= 0 {
		limit = 15
	}
	if limit > 30 {
		limit = 30
	}

	tokens := tokenizeSearchQuery(query)
	if len(tokens) == 0 {
		return []SearchItem{}, nil
	}

	normQuery := strings.Join(tokens, " ")
	ftsExpr := buildFTSPrefixExpr(tokens)

	items, err := s.searchFTS(ctx, kind, ftsExpr, normQuery, limit)
	if err == nil && len(items) > 0 {
		return items, nil
	}
	return s.searchLike(ctx, kind, tokens, normQuery, limit)
}

func (s *Store) searchFTS(ctx context.Context, kind, ftsExpr, normQuery string, limit int) ([]SearchItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT h.id, h.name, h.vendor
		FROM hardware_fts f
		JOIN hardware h ON h.id = f.id
		WHERE f.kind = ? AND hardware_fts MATCH ?
		ORDER BY
			CASE WHEN h.normalized_name = ? THEN 0
			     WHEN h.normalized_name LIKE ? THEN 1
			     ELSE 2 END,
			h.gaming_rank_score DESC
		LIMIT ?
	`, kind, ftsExpr, normQuery, normQuery+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SearchItem
	for rows.Next() {
		var item SearchItem
		if err := rows.Scan(&item.ID, &item.Name, &item.Vendor); err != nil {
			return nil, err
		}
		results = append(results, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if results == nil {
		results = []SearchItem{}
	}
	return results, nil
}

func (s *Store) searchLike(ctx context.Context, kind string, tokens []string, normQuery string, limit int) ([]SearchItem, error) {
	likePattern := "%" + strings.Join(tokens, "%") + "%"
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, vendor
		FROM hardware
		WHERE kind = ? AND normalized_name LIKE ?
		ORDER BY
			CASE WHEN normalized_name = ? THEN 0
			     WHEN normalized_name LIKE ? THEN 1
			     ELSE 2 END,
			gaming_rank_score DESC
		LIMIT ?
	`, kind, likePattern, normQuery, normQuery+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("like search: %w", err)
	}
	defer rows.Close()

	var results []SearchItem
	for rows.Next() {
		var item SearchItem
		if err := rows.Scan(&item.ID, &item.Name, &item.Vendor); err != nil {
			return nil, err
		}
		results = append(results, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if results == nil {
		results = []SearchItem{}
	}
	return results, nil
}

func tokenizeSearchQuery(q string) []string {
	var tokens []string
	var cur strings.Builder
	for _, r := range strings.ToLower(q) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

func buildFTSPrefixExpr(tokens []string) string {
	parts := make([]string, 0, len(tokens))
	for _, t := range tokens {
		parts = append(parts, t+"*")
	}
	return strings.Join(parts, " ")
}
