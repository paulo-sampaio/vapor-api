package syncer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/nargothrond-pub/vapor/api/internal/db"
	"github.com/nargothrond-pub/vapor/api/internal/parser"
)

const (
	defaultUserAgent = "VaporHardwareSync/1.0 (+https://github.com/nargothrond-pub/vapor)"
	cpuMegaURL       = "https://www.cpubenchmark.net/CPU_mega_page.html"
	cpuMegaDataURL   = "https://www.cpubenchmark.net/data/"
	cpuListURL       = "https://www.cpubenchmark.net/cpu_list.php"
	singleThreadURL  = "https://www.cpubenchmark.net/singleThread.html"
	gpuListURL       = "https://www.videocardbenchmark.net/gpu_list.php"
	gpuMegaURL       = "https://www.videocardbenchmark.net/GPU_mega_page.html"
	gpuMegaDataURL   = "https://www.videocardbenchmark.net/data/"
)

// RawEntry represents a parsed hardware benchmark record prior to DB finalization.
type RawEntry struct {
	Kind        string  `json:"kind,omitempty"`
	Name        string  `json:"name"`
	MultiScore  float64 `json:"multi_score"`
	SingleScore float64 `json:"single_score,omitempty"`
	PassMarkID  string  `json:"passmark_id,omitempty"`
}

// BuildOptions configures hardware.db generation.
type BuildOptions struct {
	OutPath  string
	SeedPath string
	MinCPUs  int
	MinGPUs  int
	Now      func() time.Time
}

// BuildResult summarizes the generated database.
type BuildResult struct {
	OutPath              string
	Source               string
	CPUCount             int
	GPUCount             int
	CPUSingleThreadCount int
}

type seedFile struct {
	CPUs []RawEntry `json:"cpus"`
	GPUs []RawEntry `json:"gpus"`
}

// LoadSeedFile reads offline hardware entries from seed_hardware.json.
func LoadSeedFile(path string) ([]RawEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read seed file %q: %w", path, err)
	}
	var sf seedFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parse seed file %q: %w", path, err)
	}
	entries := make([]RawEntry, 0, len(sf.CPUs)+len(sf.GPUs))
	for _, c := range sf.CPUs {
		c.Kind = db.KindCPU
		entries = append(entries, c)
	}
	for _, g := range sf.GPUs {
		g.Kind = db.KindGPU
		entries = append(entries, g)
	}
	return entries, nil
}

// FetchLivePassMark fetches CPU and GPU benchmark lists from PassMark politely.
// It aborts cleanly without bypassing any anti-bot challenge or rate limit.
func FetchLivePassMark(ctx context.Context) ([]RawEntry, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create cookie jar: %w", err)
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		Jar:     jar,
	}

	cpus, err := fetchLiveCPUs(ctx, client)
	if err != nil {
		return nil, err
	}
	gpus, err := fetchLiveGPUs(ctx, client)
	if err != nil {
		return nil, err
	}

	all := make([]RawEntry, 0, len(cpus)+len(gpus))
	all = append(all, cpus...)
	all = append(all, gpus...)
	return all, nil
}

func fetchLiveCPUs(ctx context.Context, client *http.Client) ([]RawEntry, error) {
	// 1. Try CPU_mega_page.html + /data/ DataTables JSON first.
	megaHTML, err := fetchWithRetry(ctx, client, cpuMegaURL, nil)
	if err == nil {
		xhrHeaders := map[string]string{
			"X-Requested-With": "XMLHttpRequest",
			"Referer":          cpuMegaURL,
			"Accept":           "application/json, text/javascript, */*; q=0.01",
		}
		if dataJSON, errData := fetchWithRetry(ctx, client, cpuMegaDataURL, xhrHeaders); errData == nil {
			if entries, errParse := ParseCPUMegaJSON(dataJSON); errParse == nil && len(entries) >= 100 {
				return entries, nil
			}
		}
		if entries, errParse := ParseCPUListHTML(bytes.NewReader(megaHTML)); errParse == nil && len(entries) >= 100 {
			enrichSingleThread(ctx, client, entries)
			return entries, nil
		}
	}

	// 2. Fallback to cpu_list.php + singleThread.html.
	listHTML, err := fetchWithRetry(ctx, client, cpuListURL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch cpu_list.php: %w", err)
	}
	entries, err := ParseCPUListHTML(bytes.NewReader(listHTML))
	if err != nil {
		return nil, fmt.Errorf("parse cpu_list.php: %w", err)
	}
	enrichSingleThread(ctx, client, entries)
	return entries, nil
}

func enrichSingleThread(ctx context.Context, client *http.Client, cpus []RawEntry) {
	stHTML, err := fetchWithRetry(ctx, client, singleThreadURL, nil)
	if err != nil {
		return
	}
	stMap, err := ParseSingleThreadHTML(bytes.NewReader(stHTML))
	if err != nil {
		return
	}
	for i := range cpus {
		if cpus[i].SingleScore > 0 {
			continue
		}
		norm := parser.NormalizeHardwareName(cpus[i].Name)
		if score, ok := stMap[norm]; ok && score > 0 {
			cpus[i].SingleScore = score
		}
	}
}

func fetchLiveGPUs(ctx context.Context, client *http.Client) ([]RawEntry, error) {
	// 1. Primary: GPU_mega_page.html + /data/ DataTables JSON (covers ~3,000+ GPUs).
	megaHTML, err := fetchWithRetry(ctx, client, gpuMegaURL, nil)
	if err == nil {
		xhrHeaders := map[string]string{
			"X-Requested-With": "XMLHttpRequest",
			"Referer":          gpuMegaURL,
			"Accept":           "application/json, text/javascript, */*; q=0.01",
		}
		if dataJSON, errData := fetchWithRetry(ctx, client, gpuMegaDataURL, xhrHeaders); errData == nil {
			if entries, errParse := ParseGPUMegaJSON(dataJSON); errParse == nil && len(entries) > 0 {
				return entries, nil
			}
		}
		if entries, errParse := ParseGPUListHTML(bytes.NewReader(megaHTML)); errParse == nil && len(entries) > 0 {
			return entries, nil
		}
	}

	// 2. Fallback: gpu_list.php
	listHTML, err := fetchWithRetry(ctx, client, gpuListURL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch gpu_list.php: %w", err)
	}
	entries, err := ParseGPUListHTML(bytes.NewReader(listHTML))
	if err != nil {
		return nil, fmt.Errorf("parse gpu_list.php: %w", err)
	}
	return entries, nil
}

func fetchWithRetry(ctx context.Context, client *http.Client, targetURL string, extraHeaders map[string]string) ([]byte, error) {
	var lastErr error
	backoff := 2 * time.Second
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", defaultUserAgent)
		req.Header.Set("Accept", "text/html,application/json;q=0.9,*/*;q=0.8")
		for k, v := range extraHeaders {
			req.Header.Set(k, v)
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("passmark returned HTTP %d for %s; aborting without bypass", resp.StatusCode, targetURL)
		}
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("http %d from %s", resp.StatusCode, targetURL)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unexpected HTTP %d from %s", resp.StatusCode, targetURL)
		}
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if isChallengePage(body) {
			return nil, fmt.Errorf("passmark returned challenge/interstitial HTML for %s; aborting cleanly", targetURL)
		}
		return body, nil
	}
	return nil, fmt.Errorf("fetch %s failed after retries: %w", targetURL, lastErr)
}

func isChallengePage(body []byte) bool {
	lower := strings.ToLower(string(body[:min(len(body), 8192)]))
	if strings.Contains(lower, "cf-browser-verification") ||
		strings.Contains(lower, "attention required! | cloudflare") ||
		strings.Contains(lower, "just a moment...") && strings.Contains(lower, "cloudflare") {
		return true
	}
	return false
}

// ParseCPUListHTML extracts CPU Name and CPU Mark from PassMark's #cputable HTML.
func ParseCPUListHTML(r io.Reader) ([]RawEntry, error) {
	return parseCPUTableHTML(r, db.KindCPU)
}

// ParseGPUListHTML extracts Videocard Name and G3D Mark from PassMark's #cputable HTML.
func ParseGPUListHTML(r io.Reader) ([]RawEntry, error) {
	return parseCPUTableHTML(r, db.KindGPU)
}

func parseCPUTableHTML(r io.Reader, kind string) ([]RawEntry, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, fmt.Errorf("parse html: %w", err)
	}
	table := findElementByID(doc, "cputable")
	if table == nil {
		return nil, errors.New("missing #cputable element in HTML")
	}

	var entries []RawEntry
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "tr" {
			cells := directChildElements(n, "td")
			if len(cells) >= 2 {
				name := strings.TrimSpace(extractText(cells[0]))
				score := parsePassMarkNumber(extractText(cells[1]))
				if name != "" && score > 0 {
					entries = append(entries, RawEntry{
						Kind:       kind,
						Name:       name,
						MultiScore: score,
					})
				}
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(table)
	return entries, nil
}

// ParseSingleThreadHTML parses PassMark singleThread.html <ul class="chartlist"> entries.
func ParseSingleThreadHTML(r io.Reader) (map[string]float64, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, fmt.Errorf("parse single thread html: %w", err)
	}
	out := make(map[string]float64)
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "li" && hasParentClass(n, "chartlist") {
			var name string
			var score float64
			var walkItem func(*html.Node)
			walkItem = func(cur *html.Node) {
				if cur.Type == html.ElementNode {
					if hasClass(cur, "prdname") && name == "" {
						name = strings.TrimSpace(extractText(cur))
					} else if hasClass(cur, "count") && score == 0 {
						score = parsePassMarkNumber(extractText(cur))
					}
				}
				for c := cur.FirstChild; c != nil; c = c.NextSibling {
					walkItem(c)
				}
			}
			walkItem(n)
			if name != "" && score > 0 {
				norm := parser.NormalizeHardwareName(name)
				if norm != "" && score > out[norm] {
					out[norm] = score
				}
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if len(out) == 0 {
		return nil, errors.New("no entries found in singleThread HTML")
	}
	return out, nil
}

// ParseCPUMegaJSON parses DataTables JSON returned by cpubenchmark.net/data/.
func ParseCPUMegaJSON(data []byte) ([]RawEntry, error) {
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal cpu mega json: %w", err)
	}
	entries := make([]RawEntry, 0, len(payload.Data))
	for _, row := range payload.Data {
		name := firstString(row, "name", "cpuName")
		multi := parsePassMarkAny(row["cpumark"])
		if multi == 0 {
			multi = parsePassMarkAny(row["mark"])
		}
		single := parsePassMarkAny(row["thread"])
		pmID := extractNumericID(row["id"])
		if name != "" && multi > 0 {
			entries = append(entries, RawEntry{
				Kind:        db.KindCPU,
				Name:        name,
				MultiScore:  multi,
				SingleScore: single,
				PassMarkID:  pmID,
			})
		}
	}
	return entries, nil
}

// ParseGPUMegaJSON parses DataTables JSON returned by videocardbenchmark.net/data/.
func ParseGPUMegaJSON(data []byte) ([]RawEntry, error) {
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal gpu mega json: %w", err)
	}
	entries := make([]RawEntry, 0, len(payload.Data))
	for _, row := range payload.Data {
		name := firstString(row, "name", "gpuName")
		multi := parsePassMarkAny(row["g3d"])
		if multi == 0 {
			multi = parsePassMarkAny(row["mark"])
		}
		pmID := extractNumericID(row["id"])
		if name != "" && multi > 0 {
			entries = append(entries, RawEntry{
				Kind:       db.KindGPU,
				Name:       name,
				MultiScore: multi,
				PassMarkID: pmID,
			})
		}
	}
	return entries, nil
}

// BuildDatabase converts RawEntry records into a finalized, immutable-ready SQLite DB.
func BuildDatabase(ctx context.Context, entries []RawEntry, opts BuildOptions) (BuildResult, error) {
	if opts.OutPath == "" {
		return BuildResult{}, errors.New("out path is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	dedup := make(map[string]db.Hardware, len(entries))
	for _, raw := range entries {
		if raw.Kind != db.KindCPU && raw.Kind != db.KindGPU {
			continue
		}
		norm := parser.NormalizeHardwareName(raw.Name)
		if norm == "" || raw.MultiScore <= 0 {
			continue
		}
		id := parser.MakeHardwareID(raw.Kind, norm)
		if existing, exists := dedup[id]; exists {
			if raw.MultiScore > existing.MultiScore {
				single := raw.SingleScore
				if single == 0 {
					single = existing.SingleScore
				}
				pmID := raw.PassMarkID
				if pmID == "" {
					pmID = existing.PassMarkID
				}
				dedup[id] = db.Hardware{
					ID:             id,
					Kind:           raw.Kind,
					Name:           strings.TrimSpace(raw.Name),
					NormalizedName: norm,
					Vendor:         parser.DetectVendor(raw.Name),
					MultiScore:     raw.MultiScore,
					SingleScore:    single,
					PassMarkID:     pmID,
				}
			} else {
				updated := false
				if existing.SingleScore == 0 && raw.SingleScore > 0 {
					existing.SingleScore = raw.SingleScore
					updated = true
				}
				if existing.PassMarkID == "" && raw.PassMarkID != "" {
					existing.PassMarkID = raw.PassMarkID
					updated = true
				}
				if updated {
					dedup[id] = existing
				}
			}
			continue
		}
		dedup[id] = db.Hardware{
			ID:             id,
			Kind:           raw.Kind,
			Name:           strings.TrimSpace(raw.Name),
			NormalizedName: norm,
			Vendor:         parser.DetectVendor(raw.Name),
			MultiScore:     raw.MultiScore,
			SingleScore:    raw.SingleScore,
			PassMarkID:     raw.PassMarkID,
		}
	}

	var cpus []db.Hardware
	var gpus []db.Hardware
	for _, h := range dedup {
		if h.Kind == db.KindCPU {
			cpus = append(cpus, h)
		} else {
			gpus = append(gpus, h)
		}
	}

	if len(cpus) < opts.MinCPUs {
		return BuildResult{}, fmt.Errorf("cpu count %d below minimum guard %d", len(cpus), opts.MinCPUs)
	}
	if len(gpus) < opts.MinGPUs {
		return BuildResult{}, fmt.Errorf("gpu count %d below minimum guard %d", len(gpus), opts.MinGPUs)
	}

	computeGamingRankScores(cpus, gpus)

	singleCount := 0
	for _, c := range cpus {
		if c.SingleScore > 0 {
			singleCount++
		}
	}

	all := make([]db.Hardware, 0, len(cpus)+len(gpus))
	all = append(all, cpus...)
	all = append(all, gpus...)
	sort.Slice(all, func(i, j int) bool {
		if all[i].Kind != all[j].Kind {
			return all[i].Kind < all[j].Kind
		}
		return all[i].ID < all[j].ID
	})

	if err := os.MkdirAll(filepath.Dir(opts.OutPath), 0o755); err != nil {
		return BuildResult{}, fmt.Errorf("mkdir %s: %w", filepath.Dir(opts.OutPath), err)
	}
	tmpPath := opts.OutPath + ".tmp"
	_ = os.Remove(tmpPath)

	conn, err := db.OpenReadWrite(tmpPath)
	if err != nil {
		return BuildResult{}, err
	}

	source := "passmark"
	if opts.SeedPath != "" {
		source = "seed"
	}

	if err := writeSQLiteDatabase(ctx, conn, all, source, len(cpus), len(gpus), singleCount, opts.Now().UTC()); err != nil {
		_ = conn.Close()
		_ = os.Remove(tmpPath)
		return BuildResult{}, err
	}
	if err := conn.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return BuildResult{}, fmt.Errorf("close tmp db: %w", err)
	}

	if _, err := os.Stat(tmpPath); err != nil {
		return BuildResult{}, fmt.Errorf("verify tmp db: %w", err)
	}
	if err := os.Rename(tmpPath, opts.OutPath); err != nil {
		_ = os.Remove(tmpPath)
		return BuildResult{}, fmt.Errorf("atomic rename %s -> %s: %w", tmpPath, opts.OutPath, err)
	}

	return BuildResult{
		OutPath:              opts.OutPath,
		Source:               source,
		CPUCount:             len(cpus),
		GPUCount:             len(gpus),
		CPUSingleThreadCount: singleCount,
	}, nil
}

func computeGamingRankScores(cpus, gpus []db.Hardware) {
	for i := range gpus {
		gpus[i].GamingRankScore = gpus[i].MultiScore
	}

	multiVals := make([]float64, 0, len(cpus))
	singleVals := make([]float64, 0, len(cpus))
	for _, c := range cpus {
		if c.MultiScore > 0 {
			multiVals = append(multiVals, c.MultiScore)
		}
		if c.SingleScore > 0 {
			singleVals = append(singleVals, c.SingleScore)
		}
	}
	mMed := median(multiVals)
	if mMed <= 0 {
		mMed = 1
	}
	sMed := median(singleVals)
	if sMed <= 0 {
		sMed = 1
	}

	for i := range cpus {
		if cpus[i].SingleScore > 0 {
			cpus[i].GamingRankScore = 0.70*(cpus[i].SingleScore/sMed) + 0.30*(cpus[i].MultiScore/mMed)
		} else {
			cpus[i].GamingRankScore = cpus[i].MultiScore / mMed
		}
	}
}

func median(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	cp := make([]float64, len(vals))
	copy(cp, vals)
	sort.Float64s(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2.0
}

func writeSQLiteDatabase(
	ctx context.Context,
	conn *sql.DB,
	all []db.Hardware,
	source string,
	cpuCount, gpuCount, singleCount int,
	nowUTC time.Time,
) error {
	if err := db.InitSchema(ctx, conn); err != nil {
		return err
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmtHW, err := tx.PrepareContext(ctx, `
		INSERT INTO hardware (id, kind, name, normalized_name, vendor, multi_score, single_score, gaming_rank_score, passmark_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("prepare insert hardware: %w", err)
	}
	defer stmtHW.Close()

	stmtFTS, err := tx.PrepareContext(ctx, `
		INSERT INTO hardware_fts (id, kind, name, normalized_name)
		VALUES (?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("prepare insert fts: %w", err)
	}
	defer stmtFTS.Close()

	for _, h := range all {
		if _, err := stmtHW.ExecContext(ctx,
			h.ID, h.Kind, h.Name, h.NormalizedName, h.Vendor,
			h.MultiScore, h.SingleScore, h.GamingRankScore, h.PassMarkID,
		); err != nil {
			return fmt.Errorf("insert hardware %s: %w", h.ID, err)
		}
		if _, err := stmtFTS.ExecContext(ctx, h.ID, h.Kind, h.Name, h.NormalizedName); err != nil {
			return fmt.Errorf("insert hardware_fts %s: %w", h.ID, err)
		}
	}

	meta := map[string]string{
		"schema_version":          "1",
		"generated_at":            nowUTC.Format(time.RFC3339),
		"source":                  source,
		"cpu_count":               strconv.Itoa(cpuCount),
		"gpu_count":               strconv.Itoa(gpuCount),
		"cpu_single_thread_count": strconv.Itoa(singleCount),
	}
	for k, v := range meta {
		if _, err := tx.ExecContext(ctx, `INSERT INTO metadata (key, value) VALUES (?, ?)`, k, v); err != nil {
			return fmt.Errorf("insert metadata %s: %w", k, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}

	// Build-time SQLite finalization per PLAN.md §3.4
	if _, err := conn.ExecContext(ctx, `INSERT INTO hardware_fts(hardware_fts) VALUES('optimize')`); err != nil {
		return fmt.Errorf("optimize fts: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA journal_mode = DELETE`); err != nil {
		return fmt.Errorf("set journal_mode=DELETE: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum db: %w", err)
	}
	var integrity string
	if err := conn.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("integrity_check: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("integrity_check failed: %q", integrity)
	}
	return nil
}

func findElementByID(n *html.Node, id string) *html.Node {
	if n.Type == html.ElementNode {
		for _, a := range n.Attr {
			if a.Key == "id" && a.Val == id {
				return n
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findElementByID(c, id); found != nil {
			return found
		}
	}
	return nil
}

func directChildElements(n *html.Node, tag string) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == tag {
			out = append(out, c)
		}
	}
	return out
}

func extractText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(cur *html.Node) {
		if cur.Type == html.TextNode {
			b.WriteString(cur.Data)
		}
		for c := cur.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

func hasClass(n *html.Node, target string) bool {
	for _, a := range n.Attr {
		if a.Key == "class" {
			for _, cls := range strings.Fields(a.Val) {
				if cls == target {
					return true
				}
			}
		}
	}
	return false
}

func hasParentClass(n *html.Node, target string) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && hasClass(p, target) {
			return true
		}
	}
	return false
}

func parsePassMarkNumber(s string) float64 {
	cleaned := strings.Map(func(r rune) rune {
		if (r >= '0' && r <= '9') || r == '.' {
			return r
		}
		return -1
	}, strings.TrimSpace(s))
	if cleaned == "" {
		return 0
	}
	v, err := strconv.ParseFloat(cleaned, 64)
	if err != nil {
		return 0
	}
	return v
}

func parsePassMarkAny(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case string:
		return parsePassMarkNumber(x)
	default:
		return 0
	}
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func extractNumericID(v any) string {
	var raw string
	switch x := v.(type) {
	case string:
		raw = strings.TrimSpace(x)
	case float64:
		if x > 0 {
			raw = strconv.FormatInt(int64(x), 10)
		}
	case int:
		if x > 0 {
			raw = strconv.Itoa(x)
		}
	}
	if raw == "" {
		return ""
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return raw
}
