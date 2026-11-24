package parser_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nargothrond-pub/vapor/api/internal/cache"
	"github.com/nargothrond-pub/vapor/api/internal/db"
	"github.com/nargothrond-pub/vapor/api/internal/parser"
	"github.com/nargothrond-pub/vapor/api/internal/syncer"
)

func loadSeedCatalog(t *testing.T) *parser.Catalog {
	t.Helper()
	entries, err := syncer.LoadSeedFile(filepath.Join("..", "..", "testdata", "seed_hardware.json"))
	if err != nil {
		t.Fatalf("LoadSeedFile: %v", err)
	}
	var items []db.Hardware
	for _, e := range entries {
		norm := parser.NormalizeHardwareName(e.Name)
		items = append(items, db.Hardware{
			ID:              parser.MakeHardwareID(e.Kind, norm),
			Kind:            e.Kind,
			Name:            e.Name,
			NormalizedName:  norm,
			Vendor:          parser.DetectVendor(e.Name),
			MultiScore:      e.MultiScore,
			SingleScore:     e.SingleScore,
			GamingRankScore: e.MultiScore,
		})
	}
	return parser.NewCatalog(items)
}

func TestSteamSpecsCorpus(t *testing.T) {
	cat := loadSeedCatalog(t)
	lru := cache.New[string, parser.ParseResult](128)

	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "steam_specs.json"))
	if err != nil {
		t.Fatalf("read steam_specs.json: %v", err)
	}
	var cases []struct {
		Name            string  `json:"name"`
		CPU             string  `json:"cpu"`
		GPU             string  `json:"gpu"`
		RAM             string  `json:"ram"`
		ExpectCPUStatus string  `json:"expect_cpu_status"`
		ExpectCPUID     string  `json:"expect_cpu_id"`
		ExpectGPUStatus string  `json:"expect_gpu_status"`
		ExpectGPUID     string  `json:"expect_gpu_id"`
		ExpectRAMGB     float64 `json:"expect_ram_gb"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("unmarshal steam_specs.json: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			cpuRes := cat.ParseSpec(db.KindCPU, tc.CPU, lru)
			if cpuRes.Status != tc.ExpectCPUStatus {
				t.Fatalf("cpu status = %q, want %q (stage=%s)", cpuRes.Status, tc.ExpectCPUStatus, cpuRes.Stage)
			}
			if tc.ExpectCPUID != "" && (cpuRes.Matched == nil || cpuRes.Matched.ID != tc.ExpectCPUID) {
				t.Fatalf("cpu matched = %+v, want %q", cpuRes.Matched, tc.ExpectCPUID)
			}
			if cpuRes.CacheHit {
				t.Fatal("expected first lookup to be cache miss")
			}

			// Second lookup must hit cache
			cpuCached := cat.ParseSpec(db.KindCPU, tc.CPU, lru)
			if !cpuCached.CacheHit {
				t.Fatal("expected second lookup to be cache hit")
			}

			gpuRes := cat.ParseSpec(db.KindGPU, tc.GPU, lru)
			if gpuRes.Status != tc.ExpectGPUStatus {
				t.Fatalf("gpu status = %q, want %q (stage=%s)", gpuRes.Status, tc.ExpectGPUStatus, gpuRes.Stage)
			}
			if tc.ExpectGPUID != "" && (gpuRes.Matched == nil || gpuRes.Matched.ID != tc.ExpectGPUID) {
				t.Fatalf("gpu matched = %+v, want %q", gpuRes.Matched, tc.ExpectGPUID)
			}

			ramGB, ok := parser.ParseRAMGB(tc.RAM)
			if tc.ExpectRAMGB > 0 && (!ok || ramGB != tc.ExpectRAMGB) {
				t.Fatalf("ram = (%v, %v), want (%v, true)", ramGB, ok, tc.ExpectRAMGB)
			}
		})
	}
}

func TestVariantNonConflation(t *testing.T) {
	cat := loadSeedCatalog(t)

	// RTX 3060 must never match RTX 3060 Ti or RTX 3060 Laptop GPU
	res := cat.ParseSpec(db.KindGPU, "NVIDIA GeForce RTX 3060", nil)
	if res.Status != parser.StatusMatched || res.Matched == nil {
		t.Fatalf("expected match for RTX 3060, got %+v", res)
	}
	if res.Matched.ID == "gpu:geforce-rtx-3060-ti" || res.Matched.ID == "gpu:geforce-rtx-3060-laptop-gpu" {
		t.Fatalf("conflated RTX 3060 with %s", res.Matched.ID)
	}

	// Ryzen 7 5800X must never match Ryzen 7 5800X3D
	cpuRes := cat.ParseSpec(db.KindCPU, "AMD Ryzen 7 5800X", nil)
	if cpuRes.Status != parser.StatusMatched || cpuRes.Matched == nil || cpuRes.Matched.ID != "cpu:amd-ryzen-7-5800x" {
		t.Fatalf("expected exact 5800X match, got %+v", cpuRes)
	}
}

func TestRAMParserEdgeCases(t *testing.T) {
	tests := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"16 GB RAM", 16, true},
		{"1,5 GB RAM", 1.5, true},
		{"512 MB RAM", 0.5, true},
		{"1 TB RAM", 1024, true},
		{"8-16 GB RAM", 8, true},
		{"9999 GB RAM", 0, false},
		{"No memory info", 0, false},
	}
	for _, tc := range tests {
		got, ok := parser.ParseRAMGB(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseRAMGB(%q) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSeriesLowestFallback(t *testing.T) {
	items := []db.Hardware{
		{ID: "gpu:geforce-9800-gtx", Kind: db.KindGPU, Name: "GeForce 9800 GTX", NormalizedName: "geforce 9800 gtx", Vendor: "NVIDIA", MultiScore: 571, GamingRankScore: 571},
		{ID: "gpu:geforce-gtx-950m", Kind: db.KindGPU, Name: "GeForce GTX 950M", NormalizedName: "geforce gtx 950m", Vendor: "NVIDIA", MultiScore: 2583, GamingRankScore: 2583},
		{ID: "gpu:geforce-gtx-950a", Kind: db.KindGPU, Name: "GeForce GTX 950A", NormalizedName: "geforce gtx 950a", Vendor: "NVIDIA", MultiScore: 2599, GamingRankScore: 2599},
		{ID: "gpu:geforce-gtx-950", Kind: db.KindGPU, Name: "GeForce GTX 950", NormalizedName: "geforce gtx 950", Vendor: "NVIDIA", MultiScore: 5353, GamingRankScore: 5353},
		{ID: "gpu:geforce-gtx-970", Kind: db.KindGPU, Name: "GeForce GTX 970", NormalizedName: "geforce gtx 970", Vendor: "NVIDIA", MultiScore: 9635, GamingRankScore: 9635},
		{ID: "gpu:quadro-rtx-3000", Kind: db.KindGPU, Name: "Quadro RTX 3000", NormalizedName: "quadro rtx 3000", Vendor: "NVIDIA", MultiScore: 10615, GamingRankScore: 10615},
		{ID: "gpu:geforce-rtx-3050", Kind: db.KindGPU, Name: "GeForce RTX 3050", NormalizedName: "geforce rtx 3050", Vendor: "NVIDIA", MultiScore: 12950, GamingRankScore: 12950},
		{ID: "gpu:geforce-rtx-3060-12gb", Kind: db.KindGPU, Name: "GeForce RTX 3060 12GB", NormalizedName: "geforce rtx 3060 12gb", Vendor: "NVIDIA", MultiScore: 17180, GamingRankScore: 17180},
		{ID: "gpu:radeon-rx-550", Kind: db.KindGPU, Name: "Radeon RX 550", NormalizedName: "radeon rx 550", Vendor: "AMD", MultiScore: 2699, GamingRankScore: 2699},
		{ID: "gpu:radeon-rx-580", Kind: db.KindGPU, Name: "Radeon RX 580", NormalizedName: "radeon rx 580", Vendor: "AMD", MultiScore: 8860, GamingRankScore: 8860},
		{ID: "gpu:radeon-rx-640", Kind: db.KindGPU, Name: "Radeon RX 640", NormalizedName: "radeon rx 640", Vendor: "AMD", MultiScore: 2275, GamingRankScore: 2275},
		{ID: "gpu:radeon-rx-6600", Kind: db.KindGPU, Name: "Radeon RX 6600", NormalizedName: "radeon rx 6600", Vendor: "AMD", MultiScore: 15240, GamingRankScore: 15240},
		{ID: "cpu:intel-core-i5-8250u", Kind: db.KindCPU, Name: "Intel Core i5-8250U", NormalizedName: "intel core i5-8250u", Vendor: "Intel", MultiScore: 5900, SingleScore: 1900, GamingRankScore: 1.22},
		{ID: "cpu:intel-core-i5-8400", Kind: db.KindCPU, Name: "Intel Core i5-8400", NormalizedName: "intel core i5-8400", Vendor: "Intel", MultiScore: 9210, SingleScore: 2440, GamingRankScore: 1.85},
		{ID: "cpu:intel-core-i5-8600k", Kind: db.KindCPU, Name: "Intel Core i5-8600K", NormalizedName: "intel core i5-8600k", Vendor: "Intel", MultiScore: 10200, SingleScore: 2650, GamingRankScore: 2.05},
		{ID: "cpu:amd-ryzen-5-5500u", Kind: db.KindCPU, Name: "AMD Ryzen 5 5500U", NormalizedName: "amd ryzen 5 5500u", Vendor: "AMD", MultiScore: 13200, SingleScore: 2500, GamingRankScore: 1.93},
		{ID: "cpu:amd-ryzen-5-5500", Kind: db.KindCPU, Name: "AMD Ryzen 5 5500", NormalizedName: "amd ryzen 5 5500", Vendor: "AMD", MultiScore: 19380, SingleScore: 3040, GamingRankScore: 2.75},
		{ID: "cpu:amd-ryzen-5-5600x", Kind: db.KindCPU, Name: "AMD Ryzen 5 5600X", NormalizedName: "amd ryzen 5 5600x", Vendor: "AMD", MultiScore: 21960, SingleScore: 3370, GamingRankScore: 3.10},
	}
	cat := parser.NewCatalog(items)

	gpuCases := []struct {
		raw       string
		wantID    string
		wantStage string
	}{
		{"GTX 900", "gpu:geforce-gtx-950", "series_lowest"},
		{"NVIDIA GeForce GTX 900 series", "gpu:geforce-gtx-950", "series_lowest"},
		{"GTX 9xx", "gpu:geforce-gtx-950", "series_lowest"},
		{"RTX 30 series", "gpu:geforce-rtx-3050", "series_lowest"},
		{"RX 500 series", "gpu:radeon-rx-550", "series_lowest"},
		{"RX 6000", "gpu:radeon-rx-6600", "series_lowest"},
		{"GeForce GTX 970", "gpu:geforce-gtx-970", "exact"},
		{"Quadro RTX 3000", "gpu:quadro-rtx-3000", "exact"},
	}
	for _, tc := range gpuCases {
		res := cat.ParseSpec(db.KindGPU, tc.raw, nil)
		if res.Status != parser.StatusMatched || res.Matched == nil {
			t.Fatalf("GPU %q unmatched: %+v", tc.raw, res)
		}
		if res.Matched.ID != tc.wantID || res.Stage != tc.wantStage {
			t.Errorf("GPU %q -> (%s, stage=%s), want (%s, stage=%s)", tc.raw, res.Matched.ID, res.Stage, tc.wantID, tc.wantStage)
		}
	}

	cpuCases := []struct {
		raw    string
		wantID string
	}{
		{"Intel Core i5 8th Gen", "cpu:intel-core-i5-8400"},
		{"AMD Ryzen 5 5000 series", "cpu:amd-ryzen-5-5500"},
	}
	for _, tc := range cpuCases {
		res := cat.ParseSpec(db.KindCPU, tc.raw, nil)
		if res.Status != parser.StatusMatched || res.Matched == nil || res.Matched.ID != tc.wantID || res.Stage != "series_lowest" {
			t.Errorf("CPU %q -> %+v, want %s (series_lowest)", tc.raw, res, tc.wantID)
		}
	}
}
