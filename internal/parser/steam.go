package parser

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/nargothrond-pub/vapor/api/internal/cache"
	"github.com/nargothrond-pub/vapor/api/internal/db"
)

const (
	StatusMatched   = "matched"
	StatusUnmatched = "unmatched"
)

// ParseResult represents the outcome of parsing and matching a Steam CPU/GPU line.
type ParseResult struct {
	Status          string            `json:"status"`
	Stage           string            `json:"stage"`
	Confidence      float64           `json:"confidence"`
	Matched         *db.Hardware      `json:"matched,omitempty"`
	Candidates      []db.Hardware     `json:"candidates,omitempty"`
	CandidateStages map[string]string `json:"candidate_stages,omitempty"`
	CacheHit        bool              `json:"cache_hit"`
}

// Catalog holds in-memory hardware lookup indexes built at API startup.
type Catalog struct {
	byID        map[string]db.Hardware
	byExactNorm map[string]map[string][]db.Hardware
	byModelKey  map[string]map[string][]db.Hardware
	allByKind   map[string][]db.Hardware
}

// NewCatalog indexes a slice of hardware rows for fast deterministic & fuzzy matching.
func NewCatalog(items []db.Hardware) *Catalog {
	c := &Catalog{
		byID: make(map[string]db.Hardware, len(items)),
		byExactNorm: map[string]map[string][]db.Hardware{
			db.KindCPU: {},
			db.KindGPU: {},
		},
		byModelKey: map[string]map[string][]db.Hardware{
			db.KindCPU: {},
			db.KindGPU: {},
		},
		allByKind: map[string][]db.Hardware{
			db.KindCPU: {},
			db.KindGPU: {},
		},
	}

	for _, h := range items {
		c.byID[h.ID] = h
		c.allByKind[h.Kind] = append(c.allByKind[h.Kind], h)
		c.byExactNorm[h.Kind][h.NormalizedName] = append(c.byExactNorm[h.Kind][h.NormalizedName], h)

		for _, key := range extractModelKeys(h.Kind, h.NormalizedName) {
			c.byModelKey[h.Kind][key] = append(c.byModelKey[h.Kind][key], h)
		}
	}
	return c
}

// GetByID returns a hardware item from the in-memory catalog.
func (c *Catalog) GetByID(id string) (db.Hardware, bool) {
	h, ok := c.byID[id]
	return h, ok
}

// ResolveUserHardware resolves user hardware from the in-memory catalog by ID
// first, falling back to exact (kind, normalized_name) match.
func (c *Catalog) ResolveUserHardware(kind, id, fallbackName string) (db.Hardware, bool) {
	if id != "" {
		if h, ok := c.byID[id]; ok && h.Kind == kind {
			return h, true
		}
	}
	if fallbackName != "" {
		norm := NormalizeHardwareName(fallbackName)
		if list := c.byExactNorm[kind][norm]; len(list) > 0 {
			return list[0], true
		}
	}
	return db.Hardware{}, false
}

// ParseSpec parses a raw Steam CPU or GPU spec string (potentially containing
// alternatives), optionally using an LRU cache.
func (c *Catalog) ParseSpec(kind, rawText string, lru *cache.LRU[string, ParseResult]) ParseResult {
	trimmed := strings.TrimSpace(rawText)
	if trimmed == "" {
		return ParseResult{Status: StatusUnmatched, Stage: "empty"}
	}

	cacheKey := kind + "\x00" + trimmed
	if lru != nil {
		if cached, ok := lru.Get(cacheKey); ok {
			cached.CacheHit = true
			return cached
		}
	}

	res := c.parseSpecUncached(kind, trimmed)
	res.CacheHit = false
	if lru != nil {
		lru.Put(cacheKey, res)
	}
	return res
}

func (c *Catalog) parseSpecUncached(kind, rawText string) ParseResult {
	segments := SplitAlternatives(rawText)
	if len(segments) == 0 {
		return ParseResult{Status: StatusUnmatched, Stage: "empty"}
	}

	var (
		matchedList []db.Hardware
		candStages  = make(map[string]string)
		bestStage   string
		bestConf    float64
		allGeneric  = true
	)

	for _, seg := range segments {
		if isGenericSpec(kind, seg) {
			continue
		}
		allGeneric = false
		hw, stage, conf, ok := c.matchSegment(kind, seg)
		if ok {
			matchedList = append(matchedList, hw)
			if _, exists := candStages[hw.ID]; !exists {
				candStages[hw.ID] = stage
			}
			if conf > bestConf {
				bestConf = conf
				bestStage = stage
			}
		}
	}

	if len(matchedList) == 0 {
		stage := "unmatched"
		if allGeneric {
			stage = "generic_rejected"
		}
		return ParseResult{Status: StatusUnmatched, Stage: stage}
	}

	// Deduplicate matched candidates by ID while preserving order.
	seen := make(map[string]struct{}, len(matchedList))
	unique := make([]db.Hardware, 0, len(matchedList))
	for _, m := range matchedList {
		if _, ok := seen[m.ID]; !ok {
			seen[m.ID] = struct{}{}
			unique = append(unique, m)
		}
	}

	// Pick the lowest gaming_rank_score as the comparison baseline per PLAN.md §4.5.
	baseline := unique[0]
	for _, cand := range unique[1:] {
		if cand.GamingRankScore < baseline.GamingRankScore {
			baseline = cand
		}
	}

	return ParseResult{
		Status:          StatusMatched,
		Stage:           bestStage,
		Confidence:      bestConf,
		Matched:         &baseline,
		Candidates:      unique,
		CandidateStages: candStages,
	}
}

var (
	reAltConnector = regexp.MustCompile(`(?i)(?:\s*/\s*|\s*\|\s*|\s*;\s*|\s*,\s*|\s+(?:or|ou|o|oder|lub|または)\s+)`)
	reBrandIntel   = regexp.MustCompile(`(?i)\b(intel\s+core|intel\s+pentium|intel\s+celeron|intel\s+xeon|intel)\b`)
	reBrandAMDCPU  = regexp.MustCompile(`(?i)\b(amd\s+ryzen|amd\s+fx|amd\s+phenom|amd\s+athlon|amd)\b`)
	reBrandNVIDIAG = regexp.MustCompile(`(?i)\b((?:nvidia\s+)?geforce\s+(?:rtx|gtx|gt)|nvidia\s+geforce|rtx|gtx|gt)\b`)
	reBrandAMDGPU  = regexp.MustCompile(`(?i)\b((?:amd\s+)?radeon\s+(?:rx|r9|r7|hd|vega)|amd\s+radeon|rx)\b`)
	reBrandIntelG  = regexp.MustCompile(`(?i)\b((?:intel\s+)?arc)\b`)
	reBareNumber   = regexp.MustCompile(`^\d{3,4}(?:\s*(?:ti|super|xt|xtx|\d+gb))?$`)
	reBareIntelCPU = regexp.MustCompile(`(?i)^i[3579][-\s]?\d{3,5}[a-z]*$`)
	reBareRyzenCPU = regexp.MustCompile(`(?i)^(?:ryzen\s+)?[3579]\s+\d{4}[a-z0-9]*$`)
)

// SplitAlternatives splits Steam requirement strings with multiple hardware
// options and carries brand/family prefixes across bare subsequent segments.
func SplitAlternatives(raw string) []string {
	parts := reAltConnector.Split(strings.TrimSpace(raw), -1)
	var out []string
	var lastPrefix string

	for _, p := range parts {
		seg := strings.TrimSpace(p)
		if seg == "" {
			continue
		}

		if lastPrefix != "" && needsPrefixCarryOver(seg) {
			seg = strings.TrimSpace(lastPrefix + " " + seg)
		}

		if prefix := detectCarryPrefix(seg); prefix != "" {
			lastPrefix = prefix
		}
		out = append(out, seg)
	}
	return out
}

func needsPrefixCarryOver(seg string) bool {
	s := strings.TrimSpace(seg)
	low := strings.ToLower(s)
	if reBareNumber.MatchString(low) || reBareIntelCPU.MatchString(low) || reBareRyzenCPU.MatchString(low) {
		return true
	}
	if strings.HasPrefix(low, "rx ") || strings.HasPrefix(low, "r9 ") || strings.HasPrefix(low, "gtx ") || strings.HasPrefix(low, "rtx ") {
		return false
	}
	return false
}

func detectCarryPrefix(seg string) string {
	switch {
	case reBrandNVIDIAG.MatchString(seg):
		return reBrandNVIDIAG.FindString(seg)
	case reBrandAMDGPU.MatchString(seg):
		return reBrandAMDGPU.FindString(seg)
	case reBrandIntelG.MatchString(seg):
		return reBrandIntelG.FindString(seg)
	case reBrandIntel.MatchString(seg):
		return reBrandIntel.FindString(seg)
	case reBrandAMDCPU.MatchString(seg):
		return reBrandAMDCPU.FindString(seg)
	default:
		return ""
	}
}

var (
	reHasModelDigit = regexp.MustCompile(`\d`)
	reGenericStrip  = regexp.MustCompile(`(?i)\b(?:dual[\s-]?core|quad[\s-]?core|hexa[\s-]?core|octa[\s-]?core|64[\s-]?bit|32[\s-]?bit|operating system|instruction set|sse2|sse3|avx2?|directx\s*\d+(?:\.\d+)?|opengl\s*\d+(?:\.\d+)?|vulkan\s*\d*(?:\.\d+)?|shader model\s*\d+(?:\.\d+)?|vram|video memory|integrated graphics|compatible|any gpu|with|memory|support|intel|amd|nvidia|cpu|gpu|processor|graphics|card|video|and|or)\b`)
	reVRAMOnly      = regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?\s*(?:gb|mb)\b`)
)

func isGenericSpec(kind, raw string) bool {
	norm := NormalizeHardwareName(raw)
	if norm == "" {
		return true
	}
	// Strip clock speeds already done in NormalizeHardwareName; also strip VRAM tokens
	// and generic capability words to see if any model identifier remains.
	stripped := reGenericStrip.ReplaceAllString(norm, " ")
	if kind == db.KindGPU {
		stripped = reVRAMOnly.ReplaceAllString(stripped, " ")
	}
	stripped = strings.TrimSpace(reMultiSpace.ReplaceAllString(stripped, " "))
	if stripped == "" {
		return true
	}
	if !reHasModelDigit.MatchString(stripped) {
		return true
	}
	return false
}

func (c *Catalog) matchSegment(kind, seg string) (db.Hardware, string, float64, bool) {
	norm := NormalizeHardwareName(seg)
	if norm == "" {
		return db.Hardware{}, "", 0, false
	}

	// Stage 1: Exact Normalized Match (+ inferred brand prefixes)
	for _, candidateNorm := range expandBrandPrefixes(kind, norm) {
		if matches := c.byExactNorm[kind][candidateNorm]; len(matches) > 0 {
			best := pickPreferredCandidate(kind, norm, matches)
			return best, "exact", 1.0, true
		}
	}

	// If the spec is an explicit series requirement (e.g. "GTX 900", "RX 500 series",
	// "Core i5 8th Gen"), resolve the lowest desktop tier in that series before
	// Stage 2/3 so bare numbers like "900" or "500" never match unrelated models
	// (e.g. "Quadro4 900 XGL" or "RTX PRO 500").
	if hw, ok := c.matchSeriesLowest(kind, norm); ok {
		return hw, "series_lowest", 0.70, true
	}

	// Stage 2: Canonical Model Key + Strict Variant Disambiguation
	keys := extractModelKeys(kind, norm)
	var pool []db.Hardware
	seenID := make(map[string]struct{})
	for _, k := range keys {
		for _, cand := range c.byModelKey[kind][k] {
			if _, ok := seenID[cand.ID]; !ok {
				seenID[cand.ID] = struct{}{}
				pool = append(pool, cand)
			}
		}
	}

	if len(pool) > 0 {
		var valid []db.Hardware
		for _, cand := range pool {
			if variantsCompatible(kind, norm, cand.NormalizedName) {
				valid = append(valid, cand)
			}
		}
		if len(valid) > 0 {
			best := pickPreferredCandidate(kind, norm, valid)
			conf := 0.94
			if best.NormalizedName == norm {
				conf = 0.98
			}
			return best, "canonical", conf, true
		}
	}

	// Stage 3: Bounded Token-Jaccard / Edit-Similarity Fallback
	// Restrict strictly to candidates sharing primary model number token to prevent
	// conflating e.g. RTX 3060 with RTX 3070 or i5-8400 with i5-9400.
	if primaryNum := extractPrimaryModelNumber(norm); primaryNum != "" {
		var (
			bestHW    db.Hardware
			bestScore float64
		)
		for _, cand := range c.allByKind[kind] {
			if extractPrimaryModelNumber(cand.NormalizedName) != primaryNum {
				continue
			}
			if !variantsCompatible(kind, norm, cand.NormalizedName) {
				continue
			}
			score := similarityScore(norm, cand.NormalizedName)
			if score >= 0.78 && score > bestScore {
				bestScore = score
				bestHW = cand
			}
		}
		if bestScore >= 0.78 {
			if bestScore > 0.89 {
				bestScore = 0.89
			}
			return bestHW, "fuzzy", bestScore, true
		}
	}

	return db.Hardware{}, "", 0, false
}

type gpuSeriesQuery struct {
	family         string
	minNum         int
	maxNum         int
	expectedDigits int
	wantMobile     bool
	wantVRAM       string
}

type cpuSeriesQuery struct {
	brand      string // "intel" or "amd"
	tier       string // "3", "5", "7", "9", or ""
	minNum     int
	maxNum     int
	wantMobile bool
}

var (
	reGPUSeriesBase = regexp.MustCompile(`\b(gtx|rtx|gt|rx|r9|r7|hd|arc)\s+(?:(\d{3,4})|(\d{1,2})x{2,3}|(\d{1,2})\s+series)\b`)
	reCPUIntelGen   = regexp.MustCompile(`\bcore\s+i([3579])(?:\s*-\s*|\s+)(\d{1,2})(?:st|nd|rd|th)\s+gen\b`)
	reCPUIntelBase  = regexp.MustCompile(`\b(?:core\s+)?i([3579])(?:\s*-\s*|\s+)(\d{1,2})(?:000|xxx)(?:\s+series)?\b`)
	reCPURyzenBase  = regexp.MustCompile(`\bryzen(?:\s+([3579]))?\s+(\d)(?:000|xxx)(?:\s+series)?\b`)
	reCandCPUIntel  = regexp.MustCompile(`\bintel\s+core\s+i([3579])-(\d{4,5})([a-z0-9]*)\b`)
	reCandCPURyzen  = regexp.MustCompile(`\bamd\s+ryzen\s+([3579])\s+(\d{4})([a-z0-9]*)\b`)
)

func (c *Catalog) matchSeriesLowest(kind, norm string) (db.Hardware, bool) {
	if kind == db.KindGPU {
		q, ok := parseGPUSeries(norm)
		if !ok {
			return db.Hardware{}, false
		}
		return c.findLowestGPUInSeries(q)
	}
	if kind == db.KindCPU {
		q, ok := parseCPUSeries(norm)
		if !ok {
			return db.Hardware{}, false
		}
		return c.findLowestCPUInSeries(q)
	}
	return db.Hardware{}, false
}

func parseGPUSeries(norm string) (gpuSeriesQuery, bool) {
	m := reGPUSeriesBase.FindStringSubmatch(norm)
	if len(m) == 0 {
		return gpuSeriesQuery{}, false
	}
	family := m[1]
	wantMobile := hasMobileIndicator(norm)
	wantVRAM := extractVRAMToken(norm)

	// Case 1: explicit round base like 900, 1000, 3000, 6000
	if m[2] != "" {
		base, err := strconv.Atoi(m[2])
		if err != nil {
			return gpuSeriesQuery{}, false
		}
		if len(m[2]) == 3 && base%100 == 0 && base >= 200 {
			return gpuSeriesQuery{family, base + 1, base + 99, 3, wantMobile, wantVRAM}, true
		}
		if len(m[2]) == 4 && base%1000 == 0 && base >= 1000 {
			return gpuSeriesQuery{family, base + 1, base + 999, 4, wantMobile, wantVRAM}, true
		}
		return gpuSeriesQuery{}, false
	}

	// Case 2: wildcard like 9xx, 30xx, 6xxx
	if m[3] != "" {
		prefix, err := strconv.Atoi(m[3])
		if err != nil || prefix <= 0 {
			return gpuSeriesQuery{}, false
		}
		if len(m[3]) == 1 {
			// e.g. 9xx (3 digits) or 6xxx (4 digits)
			if strings.Contains(m[0], "xxx") {
				base := prefix * 1000
				return gpuSeriesQuery{family, base + 1, base + 999, 4, wantMobile, wantVRAM}, true
			}
			base := prefix * 100
			return gpuSeriesQuery{family, base + 1, base + 99, 3, wantMobile, wantVRAM}, true
		}
		// 2-digit prefix like 30xx -> 3000..3099
		base := prefix * 100
		return gpuSeriesQuery{family, base + 1, base + 99, 4, wantMobile, wantVRAM}, true
	}

	// Case 3: "<N> series", e.g. "30 series" -> 3000..3099, "10 series" -> 1000..1099, "9 series" -> 900..999
	if m[4] != "" {
		prefix, err := strconv.Atoi(m[4])
		if err != nil || prefix <= 0 {
			return gpuSeriesQuery{}, false
		}
		if prefix < 10 {
			base := prefix * 100
			return gpuSeriesQuery{family, base + 1, base + 99, 3, wantMobile, wantVRAM}, true
		}
		base := prefix * 100
		return gpuSeriesQuery{family, base + 1, base + 99, 4, wantMobile, wantVRAM}, true
	}

	return gpuSeriesQuery{}, false
}

func (c *Catalog) findLowestGPUInSeries(q gpuSeriesQuery) (db.Hardware, bool) {
	reCand := regexp.MustCompile(`\b` + regexp.QuoteMeta(q.family) + `\s+(\d{3,4})([a-z]*)\b`)
	type ranked struct {
		hw  db.Hardware
		num int
	}
	var pool []ranked
	for _, cand := range c.allByKind[db.KindGPU] {
		if strings.Contains(cand.Name, "/") || strings.Contains(cand.NormalizedName, "ryzen") {
			continue
		}
		if strings.Contains(cand.NormalizedName, "quadro") ||
			strings.Contains(cand.NormalizedName, "tesla") ||
			strings.Contains(cand.NormalizedName, "nvs") ||
			strings.Contains(cand.NormalizedName, "grid") {
			continue
		}
		if !q.wantMobile {
			if hasMobileIndicator(cand.NormalizedName) ||
				strings.Contains(cand.NormalizedName, "embedded") ||
				strings.Contains(cand.NormalizedName, "oem") {
				continue
			}
		}
		m := reCand.FindStringSubmatch(cand.NormalizedName)
		if len(m) == 0 || len(m[1]) != q.expectedDigits {
			continue
		}
		suf := m[2]
		if !q.wantMobile && (suf == "m" || suf == "a" || suf == "mx" || suf == "xm") {
			continue
		}
		num, err := strconv.Atoi(m[1])
		if err != nil || num < q.minNum || num > q.maxNum {
			continue
		}
		pool = append(pool, ranked{hw: cand, num: num})
	}
	if len(pool) == 0 {
		return db.Hardware{}, false
	}

	// If VRAM was specified in the query, prefer candidates with matching VRAM.
	if q.wantVRAM != "" {
		var vramFiltered []ranked
		for _, r := range pool {
			if extractVRAMToken(r.hw.NormalizedName) == q.wantVRAM {
				vramFiltered = append(vramFiltered, r)
			}
		}
		if len(vramFiltered) > 0 {
			pool = vramFiltered
		}
	}

	sort.Slice(pool, func(i, j int) bool {
		if pool[i].hw.GamingRankScore != pool[j].hw.GamingRankScore {
			return pool[i].hw.GamingRankScore < pool[j].hw.GamingRankScore
		}
		return pool[i].num < pool[j].num
	})
	return pool[0].hw, true
}

func parseCPUSeries(norm string) (cpuSeriesQuery, bool) {
	wantMobile := hasMobileIndicator(norm)
	if m := reCPUIntelGen.FindStringSubmatch(norm); len(m) == 3 {
		gen, err := strconv.Atoi(m[2])
		if err == nil && gen >= 1 && gen <= 20 {
			return cpuSeriesQuery{
				brand:      "intel",
				tier:       m[1],
				minNum:     gen * 1000,
				maxNum:     gen*1000 + 999,
				wantMobile: wantMobile,
			}, true
		}
	}
	if m := reCPUIntelBase.FindStringSubmatch(norm); len(m) == 3 {
		gen, err := strconv.Atoi(m[2])
		if err == nil && gen >= 1 && gen <= 20 {
			return cpuSeriesQuery{
				brand:      "intel",
				tier:       m[1],
				minNum:     gen * 1000,
				maxNum:     gen*1000 + 999,
				wantMobile: wantMobile,
			}, true
		}
	}
	if m := reCPURyzenBase.FindStringSubmatch(norm); len(m) == 3 {
		gen, err := strconv.Atoi(m[2])
		if err == nil && gen >= 1 && gen <= 9 {
			return cpuSeriesQuery{
				brand:      "amd",
				tier:       m[1],
				minNum:     gen * 1000,
				maxNum:     gen*1000 + 999,
				wantMobile: wantMobile,
			}, true
		}
	}
	return cpuSeriesQuery{}, false
}

var mobileCPUSuffixes = map[string]struct{}{
	"u": {}, "ue": {}, "uc": {}, "ul": {},
	"h": {}, "hx": {}, "hs": {}, "hq": {}, "hk": {},
	"p": {}, "y": {}, "c": {}, "t": {}, "te": {},
	"ge": {}, "e": {}, "m": {}, "mq": {}, "qm": {}, "b": {}, "l": {},
}

func (c *Catalog) findLowestCPUInSeries(q cpuSeriesQuery) (db.Hardware, bool) {
	type ranked struct {
		hw  db.Hardware
		num int
	}
	var pool []ranked
	for _, cand := range c.allByKind[db.KindCPU] {
		if !q.wantMobile {
			if hasMobileIndicator(cand.NormalizedName) ||
				strings.Contains(cand.NormalizedName, "embedded") ||
				strings.Contains(cand.NormalizedName, "with radeon") ||
				strings.Contains(cand.NormalizedName, " pro ") {
				continue
			}
		}

		var m []string
		if q.brand == "intel" {
			m = reCandCPUIntel.FindStringSubmatch(cand.NormalizedName)
		} else {
			m = reCandCPURyzen.FindStringSubmatch(cand.NormalizedName)
		}
		if len(m) != 4 {
			continue
		}
		if q.tier != "" && m[1] != q.tier {
			continue
		}
		num, err := strconv.Atoi(m[2])
		if err != nil || num < q.minNum || num > q.maxNum {
			continue
		}
		if !q.wantMobile {
			if _, isMobSuf := mobileCPUSuffixes[m[3]]; isMobSuf {
				continue
			}
		}
		pool = append(pool, ranked{hw: cand, num: num})
	}
	if len(pool) == 0 {
		return db.Hardware{}, false
	}
	sort.Slice(pool, func(i, j int) bool {
		if pool[i].hw.GamingRankScore != pool[j].hw.GamingRankScore {
			return pool[i].hw.GamingRankScore < pool[j].hw.GamingRankScore
		}
		return pool[i].num < pool[j].num
	})
	return pool[0].hw, true
}

func expandBrandPrefixes(kind, norm string) []string {
	out := []string{norm}
	switch kind {
	case db.KindGPU:
		if strings.HasPrefix(norm, "rtx ") || strings.HasPrefix(norm, "gtx ") || strings.HasPrefix(norm, "gt ") {
			out = append(out, "geforce "+norm, "nvidia geforce "+norm)
		} else if strings.HasPrefix(norm, "geforce ") {
			out = append(out, "nvidia "+norm)
		} else if strings.HasPrefix(norm, "rx ") || strings.HasPrefix(norm, "r9 ") || strings.HasPrefix(norm, "r7 ") || strings.HasPrefix(norm, "hd ") {
			out = append(out, "radeon "+norm, "amd radeon "+norm)
		} else if strings.HasPrefix(norm, "radeon ") {
			out = append(out, "amd "+norm)
		} else if strings.HasPrefix(norm, "arc ") {
			out = append(out, "intel "+norm)
		}
	case db.KindCPU:
		if strings.HasPrefix(norm, "i3-") || strings.HasPrefix(norm, "i5-") || strings.HasPrefix(norm, "i7-") || strings.HasPrefix(norm, "i9-") {
			out = append(out, "core "+norm, "intel core "+norm)
		} else if strings.HasPrefix(norm, "core ") {
			out = append(out, "intel "+norm)
		} else if strings.HasPrefix(norm, "ryzen ") || strings.HasPrefix(norm, "fx-") || strings.HasPrefix(norm, "phenom ") {
			out = append(out, "amd "+norm)
		}
	}
	return out
}

var (
	reModelTokenGPU = regexp.MustCompile(`\b(?:rtx|gtx|gt|rx|r9|r7|hd|arc)\s*[a-z]?\d{3,4}[a-z]*\b`)
	reModelTokenCPU = regexp.MustCompile(`\b(?:i[3579][-\s]?\d{3,5}[a-z]*|ryzen\s+[3579]\s+\d{4}[a-z0-9]*|fx-\d{4}|e\d{4}|q\d{4}|m[1-4](?:\s+(?:pro|max|ultra))?|ultra\s+[3579]\s+\d{3}[a-z]*|\d{4}[a-z]*)\b`)
	rePrimaryDigits = regexp.MustCompile(`\b[a-z]?\d{3,5}[a-z0-9]*\b`)
)

func extractModelKeys(kind, norm string) []string {
	var keys []string
	var re *regexp.Regexp
	if kind == db.KindGPU {
		re = reModelTokenGPU
	} else {
		re = reModelTokenCPU
	}
	for _, m := range re.FindAllString(norm, -1) {
		compact := strings.ReplaceAll(strings.ReplaceAll(m, " ", ""), "-", "")
		if compact != "" {
			keys = append(keys, compact)
		}
	}
	if p := extractPrimaryModelNumber(norm); p != "" {
		keys = append(keys, p)
	}
	return keys
}

func extractPrimaryModelNumber(norm string) string {
	// Return the main numeric model token (e.g., "3060", "5600x", "12400f", "8400")
	// skipping VRAM tokens like "6gb", "12gb".
	for _, tok := range strings.Fields(norm) {
		if isVRAMToken(tok) {
			continue
		}
		if strings.HasPrefix(tok, "i3-") || strings.HasPrefix(tok, "i5-") || strings.HasPrefix(tok, "i7-") || strings.HasPrefix(tok, "i9-") {
			return strings.SplitN(tok, "-", 2)[1]
		}
		if strings.HasPrefix(tok, "fx-") {
			return strings.SplitN(tok, "-", 2)[1]
		}
		if rePrimaryDigits.MatchString(tok) && len(tok) >= 3 {
			return tok
		}
	}
	return ""
}

var (
	gpuModifiers = map[string]struct{}{
		"ti": {}, "super": {}, "xt": {}, "xtx": {}, "gre": {}, "le": {}, "se": {},
		"m": {}, "mobile": {}, "laptop": {}, "max-q": {},
	}
	cpuSuffixes = []string{"x3d", "kf", "ks", "hx", "hs", "xt", "x", "k", "f", "t", "u", "h", "g", "ge"}
)

func variantsCompatible(kind, queryNorm, candNorm string) bool {
	if kind == db.KindGPU {
		qMods := extractGPUModifiers(queryNorm)
		cMods := extractGPUModifiers(candNorm)
		if !stringSetsEqual(qMods, cMods) {
			return false
		}
		qVRAM := extractVRAMToken(queryNorm)
		cVRAM := extractVRAMToken(candNorm)
		if qVRAM != "" && cVRAM != "" && qVRAM != cVRAM {
			return false
		}
		return true
	}

	// CPU variant guard: compare CPU model suffix (x3d, x, k, kf, f, etc.)
	// and mobile/laptop indicators.
	qMobile := hasMobileIndicator(queryNorm)
	cMobile := hasMobileIndicator(candNorm)
	if qMobile != cMobile {
		return false
	}
	qSuf := extractCPUSuffix(queryNorm)
	cSuf := extractCPUSuffix(candNorm)
	return qSuf == cSuf
}

func extractGPUModifiers(norm string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, tok := range strings.Fields(norm) {
		if _, ok := gpuModifiers[tok]; ok {
			if tok == "mobile" || tok == "laptop" || tok == "max-q" {
				set["mobile"] = struct{}{}
			} else {
				set[tok] = struct{}{}
			}
		}
	}
	return set
}

func extractVRAMToken(norm string) string {
	for _, tok := range strings.Fields(norm) {
		if isVRAMToken(tok) {
			return tok
		}
	}
	return ""
}

func isVRAMToken(tok string) bool {
	if !strings.HasSuffix(tok, "gb") || len(tok) < 3 {
		return false
	}
	numPart := tok[:len(tok)-2]
	for _, r := range numPart {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func hasMobileIndicator(norm string) bool {
	return strings.Contains(norm, "laptop") ||
		strings.Contains(norm, "mobile") ||
		strings.Contains(norm, "max-q") ||
		strings.Contains(norm, "notebook")
}

func extractCPUSuffix(norm string) string {
	numTok := extractPrimaryModelNumber(norm)
	if numTok == "" {
		return ""
	}
	for _, suf := range cpuSuffixes {
		if strings.HasSuffix(numTok, suf) {
			return suf
		}
	}
	return ""
}

func pickPreferredCandidate(kind, queryNorm string, cands []db.Hardware) db.Hardware {
	if len(cands) == 1 {
		return cands[0]
	}
	cp := make([]db.Hardware, len(cands))
	copy(cp, cands)

	queryMobile := hasMobileIndicator(queryNorm)
	queryVRAM := extractVRAMToken(queryNorm)

	sort.Slice(cp, func(i, j int) bool {
		// 1. Desktop preference when query doesn't ask for mobile
		if !queryMobile {
			iMob := hasMobileIndicator(cp[i].NormalizedName)
			jMob := hasMobileIndicator(cp[j].NormalizedName)
			if iMob != jMob {
				return !iMob
			}
		}
		// 2. Exact VRAM match if query specifies VRAM
		if kind == db.KindGPU && queryVRAM != "" {
			iHas := extractVRAMToken(cp[i].NormalizedName) == queryVRAM
			jHas := extractVRAMToken(cp[j].NormalizedName) == queryVRAM
			if iHas != jHas {
				return iHas
			}
		}
		// 3. Higher gaming_rank_score (flagship desktop variant) when VRAM omitted
		return cp[i].GamingRankScore > cp[j].GamingRankScore
	})
	return cp[0]
}

func stringSetsEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func similarityScore(a, b string) float64 {
	toksA := strings.Fields(a)
	toksB := strings.Fields(b)
	setA := make(map[string]struct{}, len(toksA))
	setB := make(map[string]struct{}, len(toksB))
	for _, t := range toksA {
		setA[t] = struct{}{}
	}
	for _, t := range toksB {
		setB[t] = struct{}{}
	}
	inter := 0
	union := len(setA)
	for k := range setB {
		if _, ok := setA[k]; ok {
			inter++
		} else {
			union++
		}
	}
	jaccard := 0.0
	if union > 0 {
		jaccard = float64(inter) / float64(union)
	}

	dist := levenshtein(a, b)
	maxLen := max(len(a), len(b))
	levSim := 1.0
	if maxLen > 0 {
		levSim = 1.0 - float64(dist)/float64(maxLen)
	}
	return 0.65*jaccard + 0.35*levSim
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := 0; j <= len(rb); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, min(curr[j-1]+1, prev[j-1]+cost))
		}
		copy(prev, curr)
	}
	return prev[len(rb)]
}
