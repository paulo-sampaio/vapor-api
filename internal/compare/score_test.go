package compare_test

import (
	"testing"

	"github.com/paulo-sampaio/vapor-api/internal/compare"
	"github.com/paulo-sampaio/vapor-api/internal/db"
	"github.com/paulo-sampaio/vapor-api/internal/parser"
)

func TestEvaluateTierBlendedAndStatusMatrix(t *testing.T) {
	userCPU := db.Hardware{ID: "cpu:user", Name: "User CPU", MultiScore: 20000, SingleScore: 3400}
	userGPU := db.Hardware{ID: "gpu:user", Name: "User GPU", MultiScore: 20000}

	specCPU := db.Hardware{ID: "cpu:spec", Name: "Spec CPU", MultiScore: 10000, SingleScore: 1700}
	specGPU := db.Hardware{ID: "gpu:spec", Name: "Spec GPU", MultiScore: 10000}

	cpuParse := parser.ParseResult{
		Status:     parser.StatusMatched,
		Stage:      "exact",
		Confidence: 1.0,
		Matched:    &specCPU,
		Candidates: []db.Hardware{specCPU},
	}
	gpuParse := parser.ParseResult{
		Status:     parser.StatusMatched,
		Stage:      "exact",
		Confidence: 1.0,
		Matched:    &specGPU,
		Candidates: []db.Hardware{specGPU},
	}

	// Pass with sufficient RAM
	res := compare.EvaluateTier(userCPU, userGPU, 16, cpuParse, gpuParse, 16, true)
	if res.Summary.Status != compare.StatusPass || *res.Summary.OverallPct != 100 {
		t.Fatalf("expected pass +100%%, got %+v", res.Summary)
	}
	if res.CPU.Method != compare.MethodBlended70s30m || *res.CPU.Pct != 100 {
		t.Fatalf("expected blended_70s_30m +100%%, got %+v", res.CPU)
	}

	// Pass with RAM warning
	resWarn := compare.EvaluateTier(userCPU, userGPU, 8, cpuParse, gpuParse, 16, true)
	if resWarn.Summary.Status != compare.StatusPassRAMWarning || !resWarn.Summary.RAMWarning {
		t.Fatalf("expected pass_ram_warning, got %+v", resWarn.Summary)
	}
	if *resWarn.RAM.Pct != -50 || *resWarn.RAM.Sufficient {
		t.Fatalf("expected RAM -50%% insufficient, got %+v", resWarn.RAM)
	}

	// Multicore fallback when SingleScore == 0
	noSingleSpec := db.Hardware{ID: "cpu:old", Name: "Old CPU", MultiScore: 40000, SingleScore: 0}
	resFallback := compare.EvaluateTier(
		userCPU,
		userGPU,
		16,
		parser.ParseResult{Status: parser.StatusMatched, Matched: &noSingleSpec, Candidates: []db.Hardware{noSingleSpec}},
		parser.ParseResult{Status: parser.StatusUnmatched},
		0,
		false,
	)
	if resFallback.CPU.Method != compare.MethodMulticoreFallbk || *resFallback.CPU.Pct != -50 {
		t.Fatalf("expected multicore_fallback -50%%, got %+v", resFallback.CPU)
	}
	if resFallback.Summary.Status != compare.StatusFail || !resFallback.Summary.Partial {
		t.Fatalf("expected partial fail, got %+v", resFallback.Summary)
	}

	// Rounding boundary: -0.4 rounds to 0 and evaluates as pass
	nearParityGPU := db.Hardware{ID: "gpu:near", Name: "Near GPU", MultiScore: 20080}
	resBoundary := compare.EvaluateTier(
		userCPU,
		userGPU,
		16,
		parser.ParseResult{Status: parser.StatusUnmatched},
		parser.ParseResult{Status: parser.StatusMatched, Matched: &nearParityGPU, Candidates: []db.Hardware{nearParityGPU}},
		16,
		true,
	)
	if *resBoundary.Summary.OverallPct != 0 || resBoundary.Summary.Status != compare.StatusPass {
		t.Fatalf("expected -0.4%% to round to 0%% pass, got %+v", resBoundary.Summary)
	}

	// Neither CPU nor GPU matched -> unavailable
	resUnavail := compare.EvaluateTier(
		userCPU,
		userGPU,
		8,
		parser.ParseResult{Status: parser.StatusUnmatched},
		parser.ParseResult{Status: parser.StatusUnmatched},
		16,
		true,
	)
	if resUnavail.Summary.Status != compare.StatusUnavailable || resUnavail.Summary.OverallPct != nil || !resUnavail.Summary.RAMWarning {
		t.Fatalf("expected unavailable with ram_warning, got %+v", resUnavail.Summary)
	}
}

func TestSameBrandComparisonPreference(t *testing.T) {
	amdCPU := db.Hardware{ID: "cpu:amd-ryzen-5-5600x", Name: "AMD Ryzen 5 5600X", Vendor: "AMD", MultiScore: 21960, SingleScore: 3370, GamingRankScore: 3.1}
	intelCPU := db.Hardware{ID: "cpu:intel-core-i5-12400f", Name: "Intel Core i5-12400F", Vendor: "Intel", MultiScore: 19530, SingleScore: 3510, GamingRankScore: 3.0}
	appleCPU := db.Hardware{ID: "cpu:apple-m2", Name: "Apple M2", Vendor: "Apple", MultiScore: 16120, SingleScore: 3980, GamingRankScore: 2.9}

	specIntelCPU := db.Hardware{ID: "cpu:intel-core-i7-6700", Name: "Intel Core i7-6700", Vendor: "Intel", MultiScore: 8080, SingleScore: 2360, GamingRankScore: 1.5}
	specAMDCPU := db.Hardware{ID: "cpu:amd-ryzen-5-1600", Name: "AMD Ryzen 5 1600", Vendor: "AMD", MultiScore: 12280, SingleScore: 2080, GamingRankScore: 1.8}

	amdGPU := db.Hardware{ID: "gpu:radeon-rx-6700-xt", Name: "Radeon RX 6700 XT", Vendor: "AMD", MultiScore: 20080, GamingRankScore: 20080}
	nvidiaGPU := db.Hardware{ID: "gpu:geforce-rtx-4070", Name: "GeForce RTX 4070", Vendor: "NVIDIA", MultiScore: 26980, GamingRankScore: 26980}

	specNVIDIAGPU := db.Hardware{ID: "gpu:geforce-gtx-950", Name: "GeForce GTX 950", Vendor: "NVIDIA", MultiScore: 5353, GamingRankScore: 5353}
	specAMDGPU := db.Hardware{ID: "gpu:radeon-rx-550", Name: "Radeon RX 550", Vendor: "AMD", MultiScore: 2699, GamingRankScore: 2699}

	cpuParse := parser.ParseResult{
		Status:          parser.StatusMatched,
		Stage:           "exact",
		Confidence:      1.0,
		Matched:         &specIntelCPU,
		Candidates:      []db.Hardware{specIntelCPU, specAMDCPU},
		CandidateStages: map[string]string{specIntelCPU.ID: "exact", specAMDCPU.ID: "exact"},
	}
	gpuParse := parser.ParseResult{
		Status:          parser.StatusMatched,
		Stage:           "series_lowest",
		Confidence:      0.70,
		Matched:         &specAMDGPU,
		Candidates:      []db.Hardware{specNVIDIAGPU, specAMDGPU},
		CandidateStages: map[string]string{specNVIDIAGPU.ID: "series_lowest", specAMDGPU.ID: "series_lowest"},
	}

	// AMD user -> compares against AMD CPU and AMD GPU
	resAMD := compare.EvaluateTier(amdCPU, amdGPU, 16, cpuParse, gpuParse, 12, true)
	if !resAMD.CPU.BrandMatched || resAMD.CPU.MatchedName != specAMDCPU.Name {
		t.Fatalf("expected AMD CPU baseline %q, got %+v", specAMDCPU.Name, resAMD.CPU)
	}
	if !resAMD.GPU.BrandMatched || resAMD.GPU.MatchedName != specAMDGPU.Name || resAMD.GPU.MatchStage != "series_lowest" {
		t.Fatalf("expected AMD GPU series_lowest baseline %q, got %+v", specAMDGPU.Name, resAMD.GPU)
	}

	// Intel + NVIDIA user -> compares against Intel CPU and NVIDIA GPU
	resIntelNV := compare.EvaluateTier(intelCPU, nvidiaGPU, 16, cpuParse, gpuParse, 12, true)
	if !resIntelNV.CPU.BrandMatched || resIntelNV.CPU.MatchedName != specIntelCPU.Name {
		t.Fatalf("expected Intel CPU baseline %q, got %+v", specIntelCPU.Name, resIntelNV.CPU)
	}
	if !resIntelNV.GPU.BrandMatched || resIntelNV.GPU.MatchedName != specNVIDIAGPU.Name {
		t.Fatalf("expected NVIDIA GPU baseline %q, got %+v", specNVIDIAGPU.Name, resIntelNV.GPU)
	}

	// Apple user (not in Steam spec) -> falls back to lowest-ranked candidate
	resApple := compare.EvaluateTier(appleCPU, amdGPU, 16, cpuParse, gpuParse, 12, true)
	if resApple.CPU.BrandMatched || resApple.CPU.MatchedName != specIntelCPU.Name {
		t.Fatalf("expected Apple fallback to lowest CPU %q with BrandMatched=false, got %+v", specIntelCPU.Name, resApple.CPU)
	}
}

func TestBuildPassMarkCompareURL(t *testing.T) {
	u := compare.BuildPassMarkCompareURL(
		db.KindCPU,
		compare.PassMarkRef{Name: "AMD Ryzen 5 5600X", PassMarkID: "3859"},
		compare.PassMarkRef{Name: "Intel Core i7-6700 @ 3.40GHz", PassMarkID: "2598"},
		compare.PassMarkRef{Name: "AMD Ryzen 7 7800X3D", PassMarkID: "5299"},
	)
	want := "https://www.cpubenchmark.net/compare/AMD-Ryzen-5-5600X-vs-Intel-Core-i7-6700-3-40GHz-vs-AMD-Ryzen-7-7800X3D/3859vs2598vs5299"
	if u != want {
		t.Fatalf("CPU compare URL = %q, want %q", u, want)
	}

	// Deduplicates identical PassMarkIDs and builds 2-way GPU URL when 3rd is empty
	g := compare.BuildPassMarkCompareURL(
		db.KindGPU,
		compare.PassMarkRef{Name: "GeForce RTX 4070", PassMarkID: "4795"},
		compare.PassMarkRef{Name: "GeForce GTX 1060 6GB", PassMarkID: "3548"},
		compare.PassMarkRef{Name: "GeForce GTX 1060 6GB", PassMarkID: "3548"},
	)
	wantGPU := "https://www.videocardbenchmark.net/compare/GeForce-RTX-4070-vs-GeForce-GTX-1060-6GB/4795vs3548"
	if g != wantGPU {
		t.Fatalf("GPU compare URL = %q, want %q", g, wantGPU)
	}

	// Fewer than 2 distinct valid IDs returns ""
	if empty := compare.BuildPassMarkCompareURL(db.KindCPU, compare.PassMarkRef{Name: "Only One", PassMarkID: "3859"}); empty != "" {
		t.Fatalf("expected empty URL for single item, got %q", empty)
	}
}
