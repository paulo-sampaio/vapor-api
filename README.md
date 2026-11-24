# Vapor API (`vapor-api`)

**Vapor API** is a lightweight, zero-CGO Go REST service paired with an immutable SQLite + FTS5 hardware benchmark catalog. It powers the [Vapor Firefox Extension (`vapor-ff`)](https://github.com/paulo-sampaio/vapor-ff) (hosted at `https://vapor.nargothrond.pub`) by resolving Steam store system requirements against PassMark CPU and GPU benchmarks and returning relative performance percentages.

## Architecture & Design Principles

- **Pure Go + Immutable SQLite:** Uses Go standard library `net/http` + `log/slog` and pure-Go `modernc.org/sqlite` (`CGO_ENABLED=0`) with FTS5 full-text search. At runtime, `hardware.db` is opened strictly read-only (`mode=ro&immutable=1&_pragma=temp_store(MEMORY)&_pragma=query_only(ON)`) and loaded into an in-memory catalog for microsecond lookups.
- **Minimalist `FROM scratch` OCI Image:** Built with `golang:1.27-trixie` and packaged into a `FROM scratch` container running as UID/GID `65532` with an embedded `-healthcheck` flag, compatible with `--read-only` rootless Podman and Kubernetes `readOnlyRootFilesystem: true`.
- **Automated PassMark Sync (`cmd/sync`):** Fetches CPU Mark, Single-Thread Rating, and GPU G3D Mark data from PassMark into `hardware.db` using deterministic `<kind>:<slug>` primary keys (so saved extension IDs remain stable across weekly rebuilds) and stores `passmark_id` for 3-way comparison links.
- **Privacy-Preserving Parse Telemetry:** Emits structured JSON `spec_parsed` records to stdout for every evaluated Steam requirement tier (app ID, OS, tier, raw strings, match stage, and confidence) without logging client IP addresses, user-agents, or user hardware selections.
- **Abuse & Origin Protection:** Strict CORS allowlisting (`https://store.steampowered.com`, `moz-extension://*`, plus `VAPOR_ALLOWED_ORIGINS`), per-IP token-bucket rate limiting, 64 KiB request body cap, and panic recovery.

## Repository Layout

```text
api/
├── cmd/
│   ├── api/main.go                 # HTTP server entrypoint, env config, -healthcheck flag, graceful shutdown
│   └── sync/main.go                # PassMark / --seed SQLite builder CLI
├── internal/
│   ├── cache/                      # Generic thread-safe bounded LRU cache for parsed Steam specs
│   ├── compare/                    # 70/30 CPU & GPU/CPU scoring, brand preference, PassMark compare URL builder
│   ├── db/                         # SQLite DDL, read-only/read-write openers, FTS5 + LIKE autocomplete search
│   ├── httpapi/                    # REST handlers, CORS, per-IP rate limiter, structured spec_parsed logging
│   ├── parser/                     # Normalization, RAM parser, alternative splitter, 4-stage Steam matcher
│   └── syncer/                     # PassMark JSON/HTML fetcher, gaming_rank_score normalizer, DB finalizer
├── testdata/                       # Seed catalog (~140 parts), Steam spec corpus, PassMark HTML fixtures
├── Containerfile                   # Multi-stage golang:1.27-trixie -> FROM scratch build
├── .containerignore
├── go.mod
└── .github/workflows/
    ├── ci.yml                      # PR & push CI: go vet, race tests, seed DB build, scratch container smoke test
    └── build-image.yml             # Main push + weekly Monday 04:00 UTC PassMark sync & GHCR publish
```

## Parsing & Scoring Model

### Steam Spec Matching (`internal/parser`)

1. **Alternative Splitting & Prefix Carry-Over:** Splits multi-option lines (`/`, `|`, `;`, `,`, `or`, `ou`, `o`, `oder`, `lub`, `または`) while preserving hyphenated model codes (`i5-8400`) and carrying brand/family prefixes across bare tokens (`"Intel Core i5-8400 / i7-7700"`, `"GTX 1070 / 1080"`).
2. **Generic Spec Rejection:** Immediately rejects unidentifiable strings (`"2.4 GHz Dual Core"`, `"DirectX 11 compatible video card"`, `"Shader Model 5.0"`).
3. **4-Stage Deterministic + Series Matcher:**
   - **Stage 1 (`exact`):** Exact normalized match with inferred brand prefixes (`rtx 3060` -> `geforce rtx 3060`).
   - **Stage 2 (`canonical`):** Canonical model key lookup with strict variant guards (never conflates `RTX 3060` with `Ti` or `Laptop GPU`, or `5800X` with `5800X3D`; prefers desktop over mobile unless specified).
   - **Stage 3 (`fuzzy`):** Bounded token-Jaccard + Levenshtein similarity (`>= 0.78`) restricted to candidates sharing the primary model number.
   - **Stage 4 (`series_lowest`):** Resolves series-only requirements (`"GTX 900"`, `"RTX 30 series"`, `"RX 500 series"`, `"Core i5 8th Gen"`, `"Ryzen 5 5000 series"`) to the lowest retail desktop tier in that series.

### Scoring & Brand Preference (`internal/compare`)

- **Relative Percentage:** `pct(user, spec) = ((user - spec) / spec) * 100`.
- **CPU Blend:** `0.70 * singlePct + 0.30 * multiPct` when both CPUs have Single-Thread ratings, otherwise `multiPct` (`multicore_fallback`).
- **Section Rollup:** `0.70 * gpuPct + 0.30 * cpuPct` (falls back to whichever component matched if partial). User RAM below spec RAM keeps the CPU/GPU score and sets `ram_warning: true`.
- **Same-Brand Preference:** When Steam lists multiple alternatives, prefers candidates matching the user's vendor (`AMD`, `Intel`, `NVIDIA`) if present; otherwise selects the candidate with the lowest `gaming_rank_score`.
- **PassMark Comparison URLs:** Bundles `[user, minimum, recommended]` PassMark IDs per OS into direct `cpubenchmark.net/compare/...` and `videocardbenchmark.net/compare/...` links.

## REST API Reference

| Method & Path | Description |
| --- | --- |
| `GET /` | `302 Found` redirect to `VAPOR_EXTENSION_URL` (AMO extension listing) |
| `GET /healthz` | Service liveness, DB readiness, and hardware catalog counts |
| `GET /api/v1/hardware/search?type=cpu\|gpu&q=...&limit=15` | FTS5 + `LIKE` hardware autocomplete (`q` length `2..80`, `limit` `1..30`) |
| `POST /api/v1/compare` | Batched comparison across `1..12` OS/tier blocks (max body `64 KiB`) |

```bash
# Health check
curl -sf http://127.0.0.1:8080/healthz

# Autocomplete search
curl -sf "http://127.0.0.1:8080/api/v1/hardware/search?type=gpu&q=rtx%204070"

# Compare user hardware against Steam tiers
curl -sf -X POST http://127.0.0.1:8080/api/v1/compare \
  -H "Content-Type: application/json" \
  -d '{
    "app_id": "1091500",
    "user": {"cpu_id": "cpu:amd-ryzen-5-5600x", "gpu_id": "gpu:geforce-rtx-4070", "ram_gb": 16},
    "tiers": [
      {"key": "win:minimum", "os": "win", "tier": "minimum", "cpu": "Intel Core i7-6700 or AMD Ryzen 5 1600", "gpu": "GeForce GTX 1060 6GB", "ram": "12 GB RAM"}
    ]
  }'
```

## Local Development

Prerequisites: Go `1.25+` (CI builds with `1.27`).

```bash
# Static analysis & race-enabled test suite
go vet ./...
go test -race -v ./...

# Build offline seed database (fast, no network)
go run ./cmd/sync --seed ./testdata/seed_hardware.json --out ./data/hardware.db

# Or sync live PassMark benchmarks
go run ./cmd/sync --out ./data/hardware.db --min-cpus 1000 --min-gpus 500

# Run API server locally
VAPOR_DB_PATH=./data/hardware.db go run ./cmd/api
```

## Containerization & Deployment

### Rootless Podman (`FROM scratch`)

`Containerfile` supports `ARG DB_SOURCE=auto|prebaked|seed` (default `auto` uses `data/hardware.db` if present, otherwise builds from `testdata/seed_hardware.json`):

```bash
# Build image using pre-synced data/hardware.db
podman build -f Containerfile --build-arg DB_SOURCE=prebaked -t vapor-api:local .

# Run completely read-only
podman run --rm -d \
  --name vapor-api \
  --read-only \
  -p 8080:8080 \
  vapor-api:local

# Built-in scratch healthcheck
podman exec vapor-api /vapor-api -healthcheck
```

### Kubernetes Deployment Notes

- **Image:** `ghcr.io/paulo-sampaio/vapor-api:latest` (automatically refreshed weekly via `.github/workflows/build-image.yml`).
- **Reverse Proxy:** Set `VAPOR_TRUST_PROXY=true` so rate limiting reads `X-Forwarded-For` / `X-Real-IP`.
- **Probes:** HTTP `GET /healthz` on port `8080`.
- **SecurityContext:** `readOnlyRootFilesystem: true`, `runAsNonRoot: true`, `runAsUser: 65532`, `allowPrivilegeEscalation: false`.

## Configuration Environment Variables

| Variable | Default | Description |
| --- | --- | --- |
| `VAPOR_ADDR` | `:8080` | TCP listen address |
| `VAPOR_DB_PATH` | `/data/hardware.db` | Path to immutable SQLite database |
| `VAPOR_ALLOWED_ORIGINS` | `""` | Extra comma-separated CORS origins |
| `VAPOR_RATE_LIMIT_RPM` | `60` | Sustained requests/minute per IP |
| `VAPOR_RATE_LIMIT_BURST` | `20` | Burst allowance per IP |
| `VAPOR_TRUST_PROXY` | `false` | Trust `X-Forwarded-For` / `X-Real-IP` behind a reverse proxy |
| `VAPOR_CACHE_SIZE` | `10000` | Max parsed-spec LRU entries |
| `VAPOR_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `VAPOR_LOG_CACHE_HITS` | `true` | Emit `spec_parsed` structured log on cache hits |
| `VAPOR_EXTENSION_URL` | `https://addons.mozilla.org/firefox/addon/vapor-steam-hardware-compare/` | Redirect target for `GET /` |

## Contributing

1. Keep third-party dependencies minimal (stdlib + `modernc.org/sqlite` + `golang.org/x/net`).
2. When fixing a Steam parsing edge case, add a regression case to `testdata/steam_specs.json` and `internal/parser/parser_test.go`.
3. Run `go vet ./... && go test -race -v ./...` before opening a PR.
4. Use [Conventional Commits](https://www.conventionalcommits.org/) (`feat(...)`, `fix(...)`, `docs`, `ci`, etc.).
