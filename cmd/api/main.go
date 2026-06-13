// Command api serves the business catalog over REST/JSON.
// Wiring only; all logic lives in internal/.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/crdzbird/cuanto_cuesta/internal/api"
	"github.com/crdzbird/cuanto_cuesta/internal/ingest"
	"github.com/crdzbird/cuanto_cuesta/internal/storage/sqlite"
)

func main() {
	if err := run(); err != nil {
		slog.Error("api failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dbPath = flag.String("db", "cuanto_cuesta.db", "SQLite database path")
		addr   = flag.String("addr", ":8080", "listen address")
		token  = flag.String("scrape-token", os.Getenv("SCRAPE_TOKEN"),
			"bearer token for POST /v1/admin/scrape; empty disables the admin endpoints")
	)
	flag.Parse()

	// Cloud Run (and most PaaS) inject the listen port via $PORT; honor it
	// over the flag default so the same binary runs locally and in the cloud.
	listenAddr := *addr
	if p := os.Getenv("PORT"); p != "" {
		listenAddr = ":" + p
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	repo, err := sqlite.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()

	cfg := api.Config{ScrapeToken: *token}
	if *token != "" {
		// The runner uses a detached context so a scrape survives the request;
		// the run itself is bounded by its page/host budgets.
		cfg.ScrapeRunner = func(rctx context.Context, opts ingest.Options) (ingest.Result, error) {
			return ingest.Run(rctx, repo, opts, logger)
		}
		logger.Info("scrape admin endpoints enabled")
	} else {
		logger.Warn("scrape admin endpoints disabled (no -scrape-token)")
	}
	srv := api.NewServer(listenAddr, repo, logger, cfg)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }() // LEAK-SAFE: exits when srv closes

	logger.Info("api listening", "addr", listenAddr, "db", *dbPath)
	select {
	case err := <-errCh:
		return err // listener failed to start or crashed
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("api stopped cleanly")
	return nil
}
