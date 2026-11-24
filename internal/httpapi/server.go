package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/nargothrond-pub/vapor/api/internal/cache"
	"github.com/nargothrond-pub/vapor/api/internal/compare"
	"github.com/nargothrond-pub/vapor/api/internal/db"
	"github.com/nargothrond-pub/vapor/api/internal/parser"
)

const (
	maxCompareBodyBytes = 64 << 10 // 64 KiB
	// DefaultExtensionURL is the Mozilla Add-ons listing URL for Vapor.
	DefaultExtensionURL = "https://addons.mozilla.org/firefox/addon/vapor-steam-hardware-compare/"
)

// Server implements the Vapor REST API handlers.
type Server struct {
	store        *db.Store
	catalog      *parser.Catalog
	cache        *cache.LRU[string, parser.ParseResult]
	logger       *slog.Logger
	meta         db.Metadata
	logCacheHits bool
	extensionURL string
}

// NewServer constructs a Server instance.
func NewServer(
	store *db.Store,
	catalog *parser.Catalog,
	lru *cache.LRU[string, parser.ParseResult],
	logger *slog.Logger,
	meta db.Metadata,
	logCacheHits bool,
	extensionURL string,
) *Server {
	extURL := strings.TrimSpace(extensionURL)
	if extURL == "" {
		extURL = DefaultExtensionURL
	}
	return &Server{
		store:        store,
		catalog:      catalog,
		cache:        lru,
		logger:       logger,
		meta:         meta,
		logCacheHits: logCacheHits,
		extensionURL: extURL,
	}
}

// Routes returns the HTTP ServeMux for the API endpoints.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleRootRedirect)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/v1/hardware/search", s.handleSearch)
	mux.HandleFunc("POST /api/v1/compare", s.handleCompare)
	return mux
}

func (s *Server) handleRootRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, s.extensionURL, http.StatusFound)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "ok",
		"db_ready":     true,
		"source":       s.meta.Source,
		"generated_at": s.meta.GeneratedAt,
		"cpu_count":    s.meta.CPUCount,
		"gpu_count":    s.meta.GPUCount,
	})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := strings.TrimSpace(q.Get("type"))
	if kind != db.KindCPU && kind != db.KindGPU {
		writeError(w, http.StatusBadRequest, "invalid_type", "Query parameter 'type' must be 'cpu' or 'gpu'.")
		return
	}

	query := strings.TrimSpace(q.Get("q"))
	if len(query) < 2 || len(query) > 80 {
		writeError(w, http.StatusBadRequest, "invalid_query", "Query parameter 'q' must be between 2 and 80 characters.")
		return
	}

	limit := 15
	if rawLimit := strings.TrimSpace(q.Get("limit")); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed < 1 || parsed > 30 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "Query parameter 'limit' must be between 1 and 30.")
			return
		}
		limit = parsed
	}

	items, err := s.store.Search(r.Context(), kind, query, limit)
	if err != nil {
		s.logger.Error("hardware_search_error", "error", err)
		writeError(w, http.StatusInternalServerError, "search_failed", "Failed to search hardware catalog.")
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, map[string]any{
		"type":  kind,
		"query": query,
		"items": items,
	})
}

type compareRequest struct {
	AppID    string        `json:"app_id"`
	PageLang string        `json:"page_lang"`
	User     userHardware  `json:"user"`
	Tiers    []tierRequest `json:"tiers"`
}

type userHardware struct {
	CPUID   string  `json:"cpu_id"`
	CPUName string  `json:"cpu_name"`
	GPUID   string  `json:"gpu_id"`
	GPUName string  `json:"gpu_name"`
	RAMGB   float64 `json:"ram_gb"`
}

type tierRequest struct {
	Key  string `json:"key"`
	OS   string `json:"os"`
	Tier string `json:"tier"`
	CPU  string `json:"cpu"`
	GPU  string `json:"gpu"`
	RAM  string `json:"ram"`
}

type resolvedItem struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	PassMarkID string `json:"passmark_id,omitempty"`
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	r.Body = http.MaxBytesReader(w, r.Body, maxCompareBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "Request body exceeds 64 KiB limit.")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_body", "Failed to read request body.")
		return
	}

	var req compareRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "Request body must be valid JSON.")
		return
	}

	if len(req.Tiers) < 1 || len(req.Tiers) > 12 {
		writeError(w, http.StatusBadRequest, "invalid_tiers", "Field 'tiers' must contain between 1 and 12 entries.")
		return
	}
	if req.User.RAMGB <= 0 || req.User.RAMGB > 4096 {
		writeError(w, http.StatusBadRequest, "invalid_ram", "Field 'user.ram_gb' must be > 0 and <= 4096.")
		return
	}
	if strings.TrimSpace(req.User.CPUID) == "" || strings.TrimSpace(req.User.GPUID) == "" {
		writeError(w, http.StatusBadRequest, "invalid_user_hardware", "Fields 'user.cpu_id' and 'user.gpu_id' are required.")
		return
	}

	userCPU, cpuOK := s.catalog.ResolveUserHardware(db.KindCPU, req.User.CPUID, req.User.CPUName)
	userGPU, gpuOK := s.catalog.ResolveUserHardware(db.KindGPU, req.User.GPUID, req.User.GPUName)
	if !cpuOK || !gpuOK {
		writeError(w, http.StatusNotFound, "hardware_not_found", "Selected CPU or GPU could not be found in catalog.")
		return
	}

	results := make(map[string]compare.TierResult, len(req.Tiers))
	osTiers := make(map[string]map[string]string)
	for _, tier := range req.Tiers {
		key := strings.TrimSpace(tier.Key)
		if key == "" {
			key = tier.OS + ":" + tier.Tier
		}

		cpuParse := s.catalog.ParseSpec(db.KindCPU, tier.CPU, s.cache)
		gpuParse := s.catalog.ParseSpec(db.KindGPU, tier.GPU, s.cache)
		specRAMGB, ramOK := parser.ParseRAMGB(tier.RAM)

		tierRes := compare.EvaluateTier(userCPU, userGPU, req.User.RAMGB, cpuParse, gpuParse, specRAMGB, ramOK)
		results[key] = tierRes

		osKey := strings.TrimSpace(tier.OS)
		if osKey == "" {
			osKey = "default"
		}
		if osTiers[osKey] == nil {
			osTiers[osKey] = make(map[string]string)
		}
		osTiers[osKey][strings.ToLower(strings.TrimSpace(tier.Tier))] = key

		if s.logCacheHits || !cpuParse.CacheHit || !gpuParse.CacheHit {
			s.emitSpecParsedLog(req.AppID, tier, cpuParse, gpuParse, specRAMGB, ramOK)
		}
	}

	userCPURef := compare.PassMarkRef{Name: userCPU.Name, PassMarkID: userCPU.PassMarkID}
	userGPURef := compare.PassMarkRef{Name: userGPU.Name, PassMarkID: userGPU.PassMarkID}
	for _, tiersByName := range osTiers {
		minKey := tiersByName["minimum"]
		recKey := tiersByName["recommended"]

		var minCPURef, recCPURef, minGPURef, recGPURef compare.PassMarkRef
		if minKey != "" {
			if r := results[minKey]; r.CPU.Status == parser.StatusMatched {
				minCPURef = compare.PassMarkRef{Name: r.CPU.MatchedName, PassMarkID: r.CPU.MatchedPassMarkID}
			}
			if r := results[minKey]; r.GPU.Status == parser.StatusMatched {
				minGPURef = compare.PassMarkRef{Name: r.GPU.MatchedName, PassMarkID: r.GPU.MatchedPassMarkID}
			}
		}
		if recKey != "" {
			if r := results[recKey]; r.CPU.Status == parser.StatusMatched {
				recCPURef = compare.PassMarkRef{Name: r.CPU.MatchedName, PassMarkID: r.CPU.MatchedPassMarkID}
			}
			if r := results[recKey]; r.GPU.Status == parser.StatusMatched {
				recGPURef = compare.PassMarkRef{Name: r.GPU.MatchedName, PassMarkID: r.GPU.MatchedPassMarkID}
			}
		}

		cpuURL := compare.BuildPassMarkCompareURL(db.KindCPU, userCPURef, minCPURef, recCPURef)
		gpuURL := compare.BuildPassMarkCompareURL(db.KindGPU, userGPURef, minGPURef, recGPURef)

		for _, k := range []string{minKey, recKey} {
			if k == "" {
				continue
			}
			res := results[k]
			if res.CPU.Status == parser.StatusMatched && cpuURL != "" {
				res.CPU.PassMarkCompareURL = cpuURL
			}
			if res.GPU.Status == parser.StatusMatched && gpuURL != "" {
				res.GPU.PassMarkCompareURL = gpuURL
			}
			results[k] = res
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"user_resolved": map[string]any{
			"cpu":    resolvedItem{ID: userCPU.ID, Name: userCPU.Name, PassMarkID: userCPU.PassMarkID},
			"gpu":    resolvedItem{ID: userGPU.ID, Name: userGPU.Name, PassMarkID: userGPU.PassMarkID},
			"ram_gb": req.User.RAMGB,
		},
		"results": results,
	})
}

func (s *Server) emitSpecParsedLog(
	appID string,
	tier tierRequest,
	cpuParse, gpuParse parser.ParseResult,
	specRAMGB float64,
	ramOK bool,
) {
	cpuMatchedID := ""
	cpuMatchedName := ""
	if cpuParse.Matched != nil {
		cpuMatchedID = cpuParse.Matched.ID
		cpuMatchedName = cpuParse.Matched.Name
	}

	gpuMatchedID := ""
	gpuMatchedName := ""
	if gpuParse.Matched != nil {
		gpuMatchedID = gpuParse.Matched.ID
		gpuMatchedName = gpuParse.Matched.Name
	}

	ramStatus := parser.StatusUnmatched
	if ramOK {
		ramStatus = parser.StatusMatched
	}

	s.logger.Info("spec_parsed",
		"app_id", appID,
		"os", tier.OS,
		"tier", tier.Tier,
		"raw_cpu", tier.CPU,
		"cpu_status", cpuParse.Status,
		"cpu_matched_id", cpuMatchedID,
		"cpu_matched_name", cpuMatchedName,
		"cpu_match_stage", cpuParse.Stage,
		"cpu_confidence", cpuParse.Confidence,
		"cpu_cache_hit", cpuParse.CacheHit,
		"raw_gpu", tier.GPU,
		"gpu_status", gpuParse.Status,
		"gpu_matched_id", gpuMatchedID,
		"gpu_matched_name", gpuMatchedName,
		"gpu_match_stage", gpuParse.Stage,
		"gpu_confidence", gpuParse.Confidence,
		"gpu_cache_hit", gpuParse.CacheHit,
		"raw_ram", tier.RAM,
		"ram_status", ramStatus,
		"ram_gb", specRAMGB,
	)
}
