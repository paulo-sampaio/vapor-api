# Vapor API

Lightweight Go + immutable SQLite REST API for comparing user hardware against Steam system requirements.

## Local Development

Run all commands from this directory (`api/`):

```bash
go vet ./...
go test -race -v ./...

# Build offline seed database
go run ./cmd/sync --seed ./testdata/seed_hardware.json --out ./data/hardware.db

# Or sync live PassMark benchmarks
go run ./cmd/sync --out ./data/hardware.db --min-cpus 1000 --min-gpus 500

# Run API server
VAPOR_DB_PATH=./data/hardware.db go run ./cmd/api
```

## Rootless Podman (`FROM scratch`)

Run from this directory (`api/`):

```bash
# Build offline-seeded image locally (or auto-use data/hardware.db if present)
podman build -f Containerfile -t vapor-api:local .

# Or build with pre-synced live PassMark data
go run ./cmd/sync --out ./data/hardware.db
podman build -f Containerfile --build-arg DB_SOURCE=prebaked -t vapor-api:local .

# Run read-only in rootless Podman
podman run --rm -d \
  --name vapor-api \
  --read-only \
  -p 8080:8080 \
  vapor-api:local

# Built-in healthcheck & smoke tests
podman exec vapor-api /vapor-api -healthcheck
curl -sf http://127.0.0.1:8080/healthz
```

## Configuration Environment Variables

| Variable | Default | Description |
| --- | --- | --- |
| `VAPOR_ADDR` | `:8080` | TCP listen address |
| `VAPOR_DB_PATH` | `/data/hardware.db` | Path to immutable SQLite database |
| `VAPOR_ALLOWED_ORIGINS` | `""` | Extra comma-separated CORS origins |
| `VAPOR_RATE_LIMIT_RPM` | `60` | Sustained requests/minute per IP |
| `VAPOR_RATE_LIMIT_BURST` | `20` | Burst allowance per IP |
| `VAPOR_TRUST_PROXY` | `false` | Trust `X-Forwarded-For` / `X-Real-IP` |
| `VAPOR_CACHE_SIZE` | `10000` | Max parsed-spec LRU entries |
| `VAPOR_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `VAPOR_LOG_CACHE_HITS` | `true` | Emit `spec_parsed` structured log on cache hits |
| `VAPOR_EXTENSION_URL` | `https://addons.mozilla.org/firefox/addon/vapor-steam-hardware-compare/` | Redirect target for `GET /` |
