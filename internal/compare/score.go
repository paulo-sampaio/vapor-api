package compare

import (
	"math"
	"strings"

	"github.com/nargothrond-pub/vapor/api/internal/db"
	"github.com/nargothrond-pub/vapor/api/internal/parser"
)

const (
	StatusPass           = "pass"
	StatusPassRAMWarning = "pass_ram_warning"
	StatusFail           = "fail"
	StatusFailRAMWarning = "fail_ram_warning"
	StatusUnavailable    = "unavailable"

	MethodBlended70s30m   = "blended_70s_30m"
	MethodMulticoreFallbk = "multicore_fallback"
)

// Weights documents the section rollup weighting.
type Weights struct {
	GPU float64 `json:"gpu"`
	CPU float64 `json:"cpu"`
}

// SummaryResult represents the section-level pill for MINIMUM or RECOMMENDED.
type SummaryResult struct {
	Status            string   `json:"status"`
	OverallPct        *int     `json:"overall_pct"`
	RAMWarning        bool     `json:"ram_warning"`
	Partial           bool     `json:"partial"`
	MissingComponents []string `json:"missing_components"`
	Weights           Weights  `json:"weights"`
}

// CPUResult represents the CPU comparison breakdown.
type CPUResult struct {
	Status              string   `json:"status"`
	Pct                 *int     `json:"pct,omitempty"`
	SinglePct           *int     `json:"single_pct,omitempty"`
	MultiPct            *int     `json:"multi_pct,omitempty"`
	Method              string   `json:"method,omitempty"`
	MatchedName         string   `json:"matched_name,omitempty"`
	MatchedPassMarkID   string   `json:"matched_passmark_id,omitempty"`
	PassMarkCompareURL  string   `json:"passmark_compare_url,omitempty"`
	MatchStage          string   `json:"match_stage,omitempty"`
	BrandMatched        bool     `json:"brand_matched,omitempty"`
	AlternativesMatched []string `json:"alternatives_matched,omitempty"`
	Confidence          float64  `json:"confidence,omitempty"`
}

// GPUResult represents the GPU comparison breakdown.
type GPUResult struct {
	Status              string   `json:"status"`
	Pct                 *int     `json:"pct,omitempty"`
	MatchedName         string   `json:"matched_name,omitempty"`
	MatchedPassMarkID   string   `json:"matched_passmark_id,omitempty"`
	PassMarkCompareURL  string   `json:"passmark_compare_url,omitempty"`
	MatchStage          string   `json:"match_stage,omitempty"`
	BrandMatched        bool     `json:"brand_matched,omitempty"`
	AlternativesMatched []string `json:"alternatives_matched,omitempty"`
	Confidence          float64  `json:"confidence,omitempty"`
}

// RAMResult represents the RAM capacity comparison breakdown.
type RAMResult struct {
	Status     string   `json:"status"`
	Pct        *int     `json:"pct,omitempty"`
	SpecGB     *float64 `json:"spec_gb,omitempty"`
	UserGB     float64  `json:"user_gb,omitempty"`
	Sufficient *bool    `json:"sufficient,omitempty"`
}

// TierResult is the complete comparison result for a single Steam requirement tier.
type TierResult struct {
	Summary SummaryResult `json:"summary"`
	CPU     CPUResult     `json:"cpu"`
	GPU     GPUResult     `json:"gpu"`
	RAM     RAMResult     `json:"ram"`
}

// RelativePct computes ((user - spec) / spec) * 100.0 for spec > 0.
func RelativePct(user, spec float64) float64 {
	if spec <= 0 {
		return 0
	}
	return ((user - spec) / spec) * 100.0
}

// RoundPct rounds a floating-point percentage to the nearest integer.
func RoundPct(v float64) int {
	return int(math.Round(v))
}

// EvaluateTier computes CPU, GPU, RAM, and weighted summary scores for a tier.
func EvaluateTier(
	userCPU, userGPU db.Hardware,
	userRAMGB float64,
	cpuParse, gpuParse parser.ParseResult,
	specRAMGB float64,
	ramOK bool,
) TierResult {
	var (
		hasCPU bool
		cpuPct float64
		cpuRes = CPUResult{Status: parser.StatusUnmatched}
	)
	if cpuParse.Status == parser.StatusMatched && cpuParse.Matched != nil {
		hasCPU = true
		specCPU, cpuBrandMatched := selectComparisonBaseline(userCPU, cpuParse)
		multiPct := RelativePct(userCPU.MultiScore, specCPU.MultiScore)
		multiRounded := RoundPct(multiPct)

		var method string
		var singlePtr *int
		if userCPU.SingleScore > 0 && specCPU.SingleScore > 0 {
			singlePct := RelativePct(userCPU.SingleScore, specCPU.SingleScore)
			sRounded := RoundPct(singlePct)
			singlePtr = &sRounded
			cpuPct = 0.70*singlePct + 0.30*multiPct
			method = MethodBlended70s30m
		} else {
			cpuPct = multiPct
			method = MethodMulticoreFallbk
		}

		cpuRounded := RoundPct(cpuPct)
		alts := make([]string, 0, len(cpuParse.Candidates))
		for _, c := range cpuParse.Candidates {
			alts = append(alts, c.Name)
		}
		cpuStage := cpuParse.Stage
		if s, ok := cpuParse.CandidateStages[specCPU.ID]; ok && s != "" {
			cpuStage = s
		}
		cpuRes = CPUResult{
			Status:              parser.StatusMatched,
			Pct:                 &cpuRounded,
			SinglePct:           singlePtr,
			MultiPct:            &multiRounded,
			Method:              method,
			MatchedName:         specCPU.Name,
			MatchedPassMarkID:   specCPU.PassMarkID,
			MatchStage:          cpuStage,
			BrandMatched:        cpuBrandMatched,
			AlternativesMatched: alts,
			Confidence:          cpuParse.Confidence,
		}
	}

	var (
		hasGPU bool
		gpuPct float64
		gpuRes = GPUResult{Status: parser.StatusUnmatched}
	)
	if gpuParse.Status == parser.StatusMatched && gpuParse.Matched != nil {
		hasGPU = true
		specGPU, gpuBrandMatched := selectComparisonBaseline(userGPU, gpuParse)
		gpuPct = RelativePct(userGPU.MultiScore, specGPU.MultiScore)
		gpuRounded := RoundPct(gpuPct)
		alts := make([]string, 0, len(gpuParse.Candidates))
		for _, c := range gpuParse.Candidates {
			alts = append(alts, c.Name)
		}
		gpuStage := gpuParse.Stage
		if s, ok := gpuParse.CandidateStages[specGPU.ID]; ok && s != "" {
			gpuStage = s
		}
		gpuRes = GPUResult{
			Status:              parser.StatusMatched,
			Pct:                 &gpuRounded,
			MatchedName:         specGPU.Name,
			MatchedPassMarkID:   specGPU.PassMarkID,
			MatchStage:          gpuStage,
			BrandMatched:        gpuBrandMatched,
			AlternativesMatched: alts,
			Confidence:          gpuParse.Confidence,
		}
	}

	ramRes := RAMResult{Status: parser.StatusUnmatched}
	ramWarning := false
	if ramOK && specRAMGB > 0 && userRAMGB > 0 {
		rPct := RoundPct(RelativePct(userRAMGB, specRAMGB))
		suff := userRAMGB >= specRAMGB
		ramWarning = !suff
		specCopy := specRAMGB
		ramRes = RAMResult{
			Status:     parser.StatusMatched,
			Pct:        &rPct,
			SpecGB:     &specCopy,
			UserGB:     userRAMGB,
			Sufficient: &suff,
		}
	}

	summary := buildSummary(hasCPU, cpuPct, hasGPU, gpuPct, ramWarning)
	return TierResult{
		Summary: summary,
		CPU:     cpuRes,
		GPU:     gpuRes,
		RAM:     ramRes,
	}
}

func selectComparisonBaseline(user db.Hardware, parseRes parser.ParseResult) (db.Hardware, bool) {
	cands := parseRes.Candidates
	if len(cands) == 0 && parseRes.Matched != nil {
		cands = []db.Hardware{*parseRes.Matched}
	}
	if len(cands) == 0 {
		return db.Hardware{}, false
	}

	if user.Vendor != "" && user.Vendor != "Other" {
		var sameBrand []db.Hardware
		for _, c := range cands {
			if strings.EqualFold(c.Vendor, user.Vendor) {
				sameBrand = append(sameBrand, c)
			}
		}
		if len(sameBrand) > 0 {
			best := sameBrand[0]
			for _, c := range sameBrand[1:] {
				if c.GamingRankScore < best.GamingRankScore {
					best = c
				}
			}
			return best, true
		}
	}

	best := cands[0]
	for _, c := range cands[1:] {
		if c.GamingRankScore < best.GamingRankScore {
			best = c
		}
	}
	return best, false
}

func buildSummary(hasCPU bool, cpuPct float64, hasGPU bool, gpuPct float64, ramWarning bool) SummaryResult {
	missing := []string{}
	if !hasCPU {
		missing = append(missing, "cpu")
	}
	if !hasGPU {
		missing = append(missing, "gpu")
	}

	weights := Weights{GPU: 0.7, CPU: 0.3}

	if !hasCPU && !hasGPU {
		return SummaryResult{
			Status:            StatusUnavailable,
			OverallPct:        nil,
			RAMWarning:        ramWarning,
			Partial:           true,
			MissingComponents: missing,
			Weights:           weights,
		}
	}

	var overall float64
	partial := false
	switch {
	case hasGPU && hasCPU:
		overall = 0.70*gpuPct + 0.30*cpuPct
	case hasGPU:
		overall = gpuPct
		partial = true
	case hasCPU:
		overall = cpuPct
		partial = true
	}

	rounded := RoundPct(overall)
	var status string
	if rounded >= 0 {
		if ramWarning {
			status = StatusPassRAMWarning
		} else {
			status = StatusPass
		}
	} else {
		if ramWarning {
			status = StatusFailRAMWarning
		} else {
			status = StatusFail
		}
	}

	return SummaryResult{
		Status:            status,
		OverallPct:        &rounded,
		RAMWarning:        ramWarning,
		Partial:           partial,
		MissingComponents: missing,
		Weights:           weights,
	}
}

// PassMarkRef identifies a hardware entry for building PassMark comparison URLs.
type PassMarkRef struct {
	Name       string
	PassMarkID string
}

// BuildPassMarkCompareURL builds a PassMark multi-component comparison URL
// (e.g. https://www.cpubenchmark.net/compare/A-vs-B-vs-C/1vs2vs3) from distinct
// numeric PassMark IDs. Returns "" if fewer than 2 distinct valid IDs exist.
func BuildPassMarkCompareURL(kind string, items ...PassMarkRef) string {
	base := "https://www.cpubenchmark.net/compare/"
	if kind == db.KindGPU {
		base = "https://www.videocardbenchmark.net/compare/"
	}

	seen := make(map[string]struct{}, len(items))
	slugs := make([]string, 0, len(items))
	ids := make([]string, 0, len(items))

	for _, item := range items {
		id := strings.TrimSpace(item.PassMarkID)
		if !isNumericString(id) {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		slug := passMarkURLSlug(item.Name)
		if slug == "" {
			continue
		}
		seen[id] = struct{}{}
		slugs = append(slugs, slug)
		ids = append(ids, id)
	}

	if len(ids) < 2 {
		return ""
	}
	return base + strings.Join(slugs, "-vs-") + "/" + strings.Join(ids, "vs")
}

func passMarkURLSlug(name string) string {
	var b strings.Builder
	prevDash := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

func isNumericString(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
