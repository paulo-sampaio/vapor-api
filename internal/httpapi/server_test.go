package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nargothrond-pub/vapor/api/internal/cache"
	"github.com/nargothrond-pub/vapor/api/internal/db"
	"github.com/nargothrond-pub/vapor/api/internal/httpapi"
	"github.com/nargothrond-pub/vapor/api/internal/parser"
	"github.com/nargothrond-pub/vapor/api/internal/syncer"
)

func setupTestHandler(t *testing.T, rpm, burst int) (http.Handler, *bytes.Buffer, func()) {
	t.Helper()
	seedPath := filepath.Join("..", "..", "testdata", "seed_hardware.json")
	entries, err := syncer.LoadSeedFile(seedPath)
	if err != nil {
		t.Fatalf("LoadSeedFile: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "hardware.db")
	_, err = syncer.BuildDatabase(context.Background(), entries, syncer.BuildOptions{
		OutPath:  dbPath,
		SeedPath: seedPath,
		MinCPUs:  10,
		MinGPUs:  10,
		Now:      func() time.Time { return time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("BuildDatabase: %v", err)
	}

	conn, err := db.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}

	store := db.NewStore(conn)
	meta, err := store.GetMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	items, err := store.LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	lru := cache.New[string, parser.ParseResult](1000)
	limiter := httpapi.NewRateLimiter(rpm, burst, false)
	srv := httpapi.NewServer(store, parser.NewCatalog(items), lru, logger, meta, true, "")

	var handler http.Handler = srv.Routes()
	handler = limiter.Middleware(handler)
	handler = httpapi.CORSMiddleware([]string{"http://localhost:3000"})(handler)
	handler = httpapi.SecurityHeadersMiddleware(handler)
	handler = httpapi.RecoverMiddleware(logger)(handler)

	cleanup := func() {
		limiter.Close()
		_ = conn.Close()
	}
	return handler, &logBuf, cleanup
}

func TestHealthzSearchCompareAndLogging(t *testing.T) {
	handler, logBuf, cleanup := setupTestHandler(t, 60, 20)
	defer cleanup()

	// 1. GET /healthz
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", rec.Code)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing nosniff header")
	}

	// 2. GET /api/v1/hardware/search
	req = httptest.NewRequest(http.MethodGet, "/api/v1/hardware/search?type=gpu&q=rtx%204070", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET search = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var searchResp struct {
		Type  string          `json:"type"`
		Items []db.SearchItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &searchResp); err != nil {
		t.Fatalf("unmarshal search: %v", err)
	}
	if len(searchResp.Items) == 0 {
		t.Fatalf("expected RTX 4070 results")
	}

	// 3. POST /api/v1/compare (with fallback cpu_name resolution)
	payload := `{
		"app_id": "1091500",
		"page_lang": "en",
		"user": {
			"cpu_id": "cpu:nonexistent-slug",
			"cpu_name": "AMD Ryzen 5 5600X",
			"gpu_id": "gpu:geforce-rtx-4070",
			"ram_gb": 16
		},
		"tiers": [
			{
				"key": "win:minimum",
				"os": "win",
				"tier": "minimum",
				"cpu": "Intel Core i7-6700 or AMD Ryzen 5 1600",
				"gpu": "GeForce GTX 1060 6GB or Radeon RX 580 8GB or Arc A380",
				"ram": "12 GB RAM"
			}
		]
	}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/compare", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://store.steampowered.com")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /compare = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://store.steampowered.com" {
		t.Fatalf("expected Steam CORS origin, got %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}

	// Verify structured spec_parsed JSON log record
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, `"msg":"spec_parsed"`) || !strings.Contains(logOutput, `"app_id":"1091500"`) {
		t.Fatalf("expected spec_parsed log entry, got: %s", logOutput)
	}
}

func TestCORSAndPayloadLimitAndRateLimit(t *testing.T) {
	handler, _, cleanup := setupTestHandler(t, 60, 2)
	defer cleanup()

	// Disallowed CORS preflight
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/compare", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disallowed OPTIONS = %d, want 403", rec.Code)
	}

	// Allowed moz-extension:// preflight
	req = httptest.NewRequest(http.MethodOptions, "/api/v1/compare", nil)
	req.Header.Set("Origin", "moz-extension://1234-5678")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("allowed moz-extension OPTIONS = %d, want 204", rec.Code)
	}

	// 413 Payload Too Large (> 64 KiB)
	oversized := strings.Repeat("a", 65*1024)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/compare", strings.NewReader(oversized))
	req.RemoteAddr = "10.0.0.1:1234"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413", rec.Code)
	}

	// Rate limit burst=2: 2nd request ok, 3rd returns 429
	req2 := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req2.RemoteAddr = "10.0.0.1:1234"
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("2nd request = %d, want 200", rec2.Code)
	}

	req3 := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req3.RemoteAddr = "10.0.0.1:1234"
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd request = %d, want 429", rec3.Code)
	}
}

func TestCompareBundlesPassMarkCompareURLs(t *testing.T) {
	items := []db.Hardware{
		{ID: "cpu:amd-ryzen-5-5600x", Kind: db.KindCPU, Name: "AMD Ryzen 5 5600X", NormalizedName: "amd ryzen 5 5600x", Vendor: "AMD", MultiScore: 21820, SingleScore: 3366, GamingRankScore: 3.1, PassMarkID: "3859"},
		{ID: "cpu:intel-core-i7-6700", Kind: db.KindCPU, Name: "Intel Core i7-6700", NormalizedName: "intel core i7-6700", Vendor: "Intel", MultiScore: 8080, SingleScore: 2360, GamingRankScore: 1.5, PassMarkID: "2598"},
		{ID: "cpu:amd-ryzen-7-7800x3d", Kind: db.KindCPU, Name: "AMD Ryzen 7 7800X3D", NormalizedName: "amd ryzen 7 7800x3d", Vendor: "AMD", MultiScore: 34320, SingleScore: 3890, GamingRankScore: 4.0, PassMarkID: "5299"},
		{ID: "gpu:geforce-rtx-4070", Kind: db.KindGPU, Name: "GeForce RTX 4070", NormalizedName: "geforce rtx 4070", Vendor: "NVIDIA", MultiScore: 26851, GamingRankScore: 26851, PassMarkID: "4795"},
		{ID: "gpu:geforce-gtx-1060-6gb", Kind: db.KindGPU, Name: "GeForce GTX 1060 6GB", NormalizedName: "geforce gtx 1060 6gb", Vendor: "NVIDIA", MultiScore: 10090, GamingRankScore: 10090, PassMarkID: "3548"},
		{ID: "gpu:geforce-rtx-2060-super", Kind: db.KindGPU, Name: "GeForce RTX 2060 SUPER", NormalizedName: "geforce rtx 2060 super", Vendor: "NVIDIA", MultiScore: 16590, GamingRankScore: 16590, PassMarkID: "4117"},
	}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	lru := cache.New[string, parser.ParseResult](100)
	srv := httpapi.NewServer(nil, parser.NewCatalog(items), lru, logger, db.Metadata{Source: "test"}, false, "")

	payload := `{
		"app_id": "1091500",
		"user": {"cpu_id": "cpu:amd-ryzen-5-5600x", "gpu_id": "gpu:geforce-rtx-4070", "ram_gb": 16},
		"tiers": [
			{"key": "win:minimum", "os": "win", "tier": "minimum", "cpu": "Intel Core i7-6700", "gpu": "GeForce GTX 1060 6GB", "ram": "12 GB RAM"},
			{"key": "win:recommended", "os": "win", "tier": "recommended", "cpu": "AMD Ryzen 7 7800X3D", "gpu": "GeForce RTX 2060 SUPER", "ram": "16 GB RAM"}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/compare", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /compare = %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Results map[string]struct {
			CPU struct {
				PassMarkCompareURL string `json:"passmark_compare_url"`
			} `json:"cpu"`
			GPU struct {
				PassMarkCompareURL string `json:"passmark_compare_url"`
			} `json:"gpu"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantCPU := "https://www.cpubenchmark.net/compare/AMD-Ryzen-5-5600X-vs-Intel-Core-i7-6700-vs-AMD-Ryzen-7-7800X3D/3859vs2598vs5299"
	wantGPU := "https://www.videocardbenchmark.net/compare/GeForce-RTX-4070-vs-GeForce-GTX-1060-6GB-vs-GeForce-RTX-2060-SUPER/4795vs3548vs4117"
	if resp.Results["win:minimum"].CPU.PassMarkCompareURL != wantCPU || resp.Results["win:recommended"].CPU.PassMarkCompareURL != wantCPU {
		t.Fatalf("unexpected CPU URLs: min=%q rec=%q", resp.Results["win:minimum"].CPU.PassMarkCompareURL, resp.Results["win:recommended"].CPU.PassMarkCompareURL)
	}
	if resp.Results["win:minimum"].GPU.PassMarkCompareURL != wantGPU || resp.Results["win:recommended"].GPU.PassMarkCompareURL != wantGPU {
		t.Fatalf("unexpected GPU URLs: min=%q rec=%q", resp.Results["win:minimum"].GPU.PassMarkCompareURL, resp.Results["win:recommended"].GPU.PassMarkCompareURL)
	}
}

func TestRootRedirectToAMO(t *testing.T) {
	handler, _, cleanup := setupTestHandler(t, 60, 20)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("GET / = %d, want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != httpapi.DefaultExtensionURL {
		t.Fatalf("GET / Location = %q, want %q", got, httpapi.DefaultExtensionURL)
	}

	req404 := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	rec404 := httptest.NewRecorder()
	handler.ServeHTTP(rec404, req404)
	if rec404.Code != http.StatusNotFound {
		t.Fatalf("GET /does-not-exist = %d, want 404", rec404.Code)
	}
}
