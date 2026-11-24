package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nargothrond-pub/vapor/api/internal/cache"
	"github.com/nargothrond-pub/vapor/api/internal/db"
	"github.com/nargothrond-pub/vapor/api/internal/httpapi"
	"github.com/nargothrond-pub/vapor/api/internal/parser"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "run local /healthz check and exit 0/1")
	flag.Parse()

	addr := envStr("VAPOR_ADDR", ":8080")
	if *healthcheck {
		os.Exit(runHealthcheck(addr))
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(envStr("VAPOR_LOG_LEVEL", "info")),
	}))

	dbPath := envStr("VAPOR_DB_PATH", "/data/hardware.db")
	conn, err := db.OpenReadOnly(dbPath)
	if err != nil {
		logger.Error("failed_to_open_db", "path", dbPath, "error", err)
		os.Exit(1)
	}
	defer conn.Close()

	store := db.NewStore(conn)
	ctx := context.Background()

	meta, err := store.GetMetadata(ctx)
	if err != nil {
		logger.Error("failed_to_read_metadata", "error", err)
		os.Exit(1)
	}

	items, err := store.LoadAll(ctx)
	if err != nil {
		logger.Error("failed_to_load_catalog", "error", err)
		os.Exit(1)
	}
	catalog := parser.NewCatalog(items)

	cacheSize := envInt("VAPOR_CACHE_SIZE", 10000)
	lru := cache.New[string, parser.ParseResult](cacheSize)

	rpm := envInt("VAPOR_RATE_LIMIT_RPM", 60)
	burst := envInt("VAPOR_RATE_LIMIT_BURST", 20)
	trustProxy := envBool("VAPOR_TRUST_PROXY", false)
	logCacheHits := envBool("VAPOR_LOG_CACHE_HITS", true)
	allowedOrigins := splitCSV(envStr("VAPOR_ALLOWED_ORIGINS", ""))
	extensionURL := envStr("VAPOR_EXTENSION_URL", httpapi.DefaultExtensionURL)

	limiter := httpapi.NewRateLimiter(rpm, burst, trustProxy)
	defer limiter.Close()

	srvAPI := httpapi.NewServer(store, catalog, lru, logger, meta, logCacheHits, extensionURL)

	var handler http.Handler = srvAPI.Routes()
	handler = limiter.Middleware(handler)
	handler = httpapi.CORSMiddleware(allowedOrigins)(handler)
	handler = httpapi.SecurityHeadersMiddleware(handler)
	handler = httpapi.RecoverMiddleware(logger)(handler)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("vapor_api_starting",
			"addr", addr,
			"source", meta.Source,
			"cpus", meta.CPUCount,
			"gpus", meta.GPUCount,
		)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil {
			logger.Error("server_error", "error", err)
			os.Exit(1)
		}
	case sig := <-sigCh:
		logger.Info("vapor_api_shutting_down", "signal", sig.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown_error", "error", err)
			os.Exit(1)
		}
	}
}

func runHealthcheck(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		port = strings.TrimPrefix(addr, ":")
		if port == "" {
			port = "8080"
		}
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/healthz", port))
	if err != nil {
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return 0
	}
	return 1
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func splitCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func parseLogLevel(raw string) slog.Level {
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
