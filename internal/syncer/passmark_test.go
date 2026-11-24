package syncer_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulo-sampaio/vapor-api/internal/db"
	"github.com/paulo-sampaio/vapor-api/internal/parser"
	"github.com/paulo-sampaio/vapor-api/internal/syncer"
)

func TestParseHTMLFixturesAndBuildDB(t *testing.T) {
	cpuFile, err := os.Open(filepath.Join("..", "..", "testdata", "passmark_cpu_list.html"))
	if err != nil {
		t.Fatalf("open cpu html: %v", err)
	}
	defer cpuFile.Close()

	cpus, err := syncer.ParseCPUListHTML(cpuFile)
	if err != nil {
		t.Fatalf("ParseCPUListHTML: %v", err)
	}
	if len(cpus) != 4 {
		t.Fatalf("expected 4 CPUs, got %d", len(cpus))
	}

	stFile, err := os.Open(filepath.Join("..", "..", "testdata", "passmark_single_thread.html"))
	if err != nil {
		t.Fatalf("open single thread html: %v", err)
	}
	defer stFile.Close()

	stMap, err := syncer.ParseSingleThreadHTML(stFile)
	if err != nil {
		t.Fatalf("ParseSingleThreadHTML: %v", err)
	}
	if len(stMap) != 4 {
		t.Fatalf("expected 4 single thread scores, got %d", len(stMap))
	}

	for i := range cpus {
		norm := parser.NormalizeHardwareName(cpus[i].Name)
		if score, ok := stMap[norm]; ok {
			cpus[i].SingleScore = score
		}
		if cpus[i].SingleScore == 0 {
			t.Fatalf("expected single thread join for %q (norm=%q)", cpus[i].Name, norm)
		}
	}

	gpuFile, err := os.Open(filepath.Join("..", "..", "testdata", "passmark_gpu_list.html"))
	if err != nil {
		t.Fatalf("open gpu html: %v", err)
	}
	defer gpuFile.Close()

	gpus, err := syncer.ParseGPUListHTML(gpuFile)
	if err != nil {
		t.Fatalf("ParseGPUListHTML: %v", err)
	}
	if len(gpus) != 4 {
		t.Fatalf("expected 4 GPUs, got %d", len(gpus))
	}

	all := append(cpus, gpus...)
	outPath := filepath.Join(t.TempDir(), "passmark_fixture.db")
	res, err := syncer.BuildDatabase(context.Background(), all, syncer.BuildOptions{
		OutPath: outPath,
		MinCPUs: 4,
		MinGPUs: 4,
	})
	if err != nil {
		t.Fatalf("BuildDatabase: %v", err)
	}
	if res.CPUCount != 4 || res.GPUCount != 4 || res.CPUSingleThreadCount != 4 {
		t.Fatalf("unexpected build result: %+v", res)
	}

	// Ensure no -wal or -shm sidecars exist
	for _, suffix := range []string{"-wal", "-shm", ".tmp"} {
		if _, err := os.Stat(outPath + suffix); !os.IsNotExist(err) {
			t.Fatalf("unexpected sidecar file %s%s", outPath, suffix)
		}
	}

	conn, err := db.OpenReadOnly(outPath)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer conn.Close()
}

func TestMinGuardFailsOnSmallInput(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "fail.db")
	_, err := syncer.BuildDatabase(context.Background(), []syncer.RawEntry{
		{Kind: db.KindCPU, Name: "Intel Core i5-12400F", MultiScore: 19530, SingleScore: 3510},
	}, syncer.BuildOptions{
		OutPath: outPath,
		MinCPUs: 10,
		MinGPUs: 10,
	})
	if err == nil {
		t.Fatal("expected min guard error, got nil")
	}
}

func TestParseSingleThreadNestedAnchorAndMegaJSON(t *testing.T) {
	nestedHTML := `<ul class="chartlist">
		<li id="rk1"><span class="more_details"></span><a href="/cpu.php"><span class="prdname">AMD Ryzen 5 5600X</span><div><span class="index"></span></div><span class="count">3,366</span></a></li>
	</ul>`
	stMap, err := syncer.ParseSingleThreadHTML(strings.NewReader(nestedHTML))
	if err != nil {
		t.Fatalf("ParseSingleThreadHTML nested: %v", err)
	}
	if stMap["amd ryzen 5 5600x"] != 3366 {
		t.Fatalf("expected 3366 for Ryzen 5 5600X, got %v", stMap)
	}

	cpuJSON := `{"data":[
		{"id":"3859","name":"AMD Ryzen 5 5600X","cpumark":"21,820","thread":"3,366"},
		{"id":"4681","name":"Intel Core i5-12400F","cpumark":"19,530","thread":"3,510"}
	]}`
	cpus, err := syncer.ParseCPUMegaJSON([]byte(cpuJSON))
	if err != nil || len(cpus) != 2 {
		t.Fatalf("ParseCPUMegaJSON: len=%d err=%v", len(cpus), err)
	}
	if cpus[0].Name != "AMD Ryzen 5 5600X" || cpus[0].MultiScore != 21820 || cpus[0].SingleScore != 3366 || cpus[0].PassMarkID != "3859" {
		t.Fatalf("unexpected first CPU: %+v", cpus[0])
	}

	gpuJSON := `{"data":[{"id":"4795","name":"GeForce RTX 4070","g3d":"26,851"}]}`
	gpus, err := syncer.ParseGPUMegaJSON([]byte(gpuJSON))
	if err != nil || len(gpus) != 1 || gpus[0].MultiScore != 26851 || gpus[0].PassMarkID != "4795" {
		t.Fatalf("ParseGPUMegaJSON: gpus=%+v err=%v", gpus, err)
	}
}
