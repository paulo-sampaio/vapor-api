package db_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/paulo-sampaio/vapor-api/internal/db"
	"github.com/paulo-sampaio/vapor-api/internal/syncer"
)

func buildTestDB(t *testing.T) string {
	t.Helper()
	seedPath := filepath.Join("..", "..", "testdata", "seed_hardware.json")
	entries, err := syncer.LoadSeedFile(seedPath)
	if err != nil {
		t.Fatalf("LoadSeedFile: %v", err)
	}
	outPath := filepath.Join(t.TempDir(), "hardware.db")
	_, err = syncer.BuildDatabase(context.Background(), entries, syncer.BuildOptions{
		OutPath:  outPath,
		SeedPath: seedPath,
		MinCPUs:  10,
		MinGPUs:  10,
		Now:      func() time.Time { return time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("BuildDatabase: %v", err)
	}
	return outPath
}

func TestStoreReadOnlyAndSearch(t *testing.T) {
	dbPath := buildTestDB(t)
	conn, err := db.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer conn.Close()

	store := db.NewStore(conn)
	ctx := context.Background()

	meta, err := store.GetMetadata(ctx)
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if meta.SchemaVersion != "1" || meta.Source != "seed" {
		t.Fatalf("unexpected metadata: %+v", meta)
	}
	if meta.CPUCount < 50 || meta.GPUCount < 40 {
		t.Fatalf("expected seeded counts, got cpu=%d gpu=%d", meta.CPUCount, meta.GPUCount)
	}

	// Deterministic slug lookup
	hw, ok, err := store.GetByID(ctx, "cpu:amd-ryzen-5-5600x")
	if err != nil || !ok {
		t.Fatalf("GetByID failed: ok=%v err=%v", ok, err)
	}
	if hw.Name != "AMD Ryzen 5 5600X" || hw.Vendor != "AMD" {
		t.Fatalf("unexpected hardware row: %+v", hw)
	}

	// Fallback normalized-name lookup when ID misses
	resolved, ok, err := store.ResolveUserHardware(ctx, db.KindGPU, "gpu:stale-slug", "geforce rtx 4070")
	if err != nil || !ok {
		t.Fatalf("ResolveUserHardware fallback failed: ok=%v err=%v", ok, err)
	}
	if resolved.ID != "gpu:geforce-rtx-4070" {
		t.Fatalf("expected gpu:geforce-rtx-4070, got %s", resolved.ID)
	}

	// FTS5 search
	items, err := store.Search(ctx, db.KindGPU, "rtx 3060", 10)
	if err != nil {
		t.Fatalf("Search GPU: %v", err)
	}
	if len(items) < 2 {
		t.Fatalf("expected multiple RTX 3060 variants, got %+v", items)
	}

	// Search CPU with hyphenated token
	cpuItems, err := store.Search(ctx, db.KindCPU, "i5-12400f", 10)
	if err != nil {
		t.Fatalf("Search CPU: %v", err)
	}
	if len(cpuItems) == 0 || cpuItems[0].ID != "cpu:intel-core-i5-12400f" {
		t.Fatalf("expected i5-12400f first, got %+v", cpuItems)
	}
}
