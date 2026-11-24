package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/paulo-sampaio/vapor-api/internal/syncer"
)

func main() {
	outPath := flag.String("out", "./data/hardware.db", "output SQLite database path")
	seedPath := flag.String("seed", "", "optional path to offline seed_hardware.json fixture")
	minCPUs := flag.Int("min-cpus", -1, "minimum CPU rows required (default: 1000 live, 10 seed)")
	minGPUs := flag.Int("min-gpus", -1, "minimum GPU rows required (default: 500 live, 10 seed)")
	flag.Parse()

	cpuGuard := *minCPUs
	gpuGuard := *minGPUs
	if cpuGuard < 0 {
		if *seedPath != "" {
			cpuGuard = 10
		} else {
			cpuGuard = 1000
		}
	}
	if gpuGuard < 0 {
		if *seedPath != "" {
			gpuGuard = 10
		} else {
			gpuGuard = 500
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var (
		entries []syncer.RawEntry
		err     error
	)
	if *seedPath != "" {
		entries, err = syncer.LoadSeedFile(*seedPath)
	} else {
		entries, err = syncer.FetchLivePassMark(ctx)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "vapor-sync error: %v\n", err)
		os.Exit(1)
	}

	res, err := syncer.BuildDatabase(ctx, entries, syncer.BuildOptions{
		OutPath:  *outPath,
		SeedPath: *seedPath,
		MinCPUs:  cpuGuard,
		MinGPUs:  gpuGuard,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "vapor-sync build error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Built %s (source=%s, cpus=%d, gpus=%d, cpu_single_thread=%d)\n",
		res.OutPath, res.Source, res.CPUCount, res.GPUCount, res.CPUSingleThreadCount)
}
