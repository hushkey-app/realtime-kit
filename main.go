// Command realtime-kit is a small HTTP sidecar over the LiveKit server SDK.
//
// It exists because the app that needs LiveKit (pakku) runs on Deno, and the
// server SDK worth using — github.com/livekit/server-sdk-go — is Go. Rather than
// reimplement token signing and the room API in TypeScript and then own that
// reimplementation forever, the app calls this service and this service calls
// the SDK.
//
// It is stateless and multi-tenant: every request names the LiveKit deployment
// and key it is acting for, so one instance serves every workspace. See
// internal/api for the trust model — in short, private network only.
//
//	REALTIME_KIT_TOKEN=…  go run .
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/hushkey-app/realtime-kit/internal/api"
	"github.com/hushkey-app/realtime-kit/internal/config"
	"github.com/hushkey-app/realtime-kit/internal/livekit"
)

// Set by the release build. Local builds deliberately identify themselves as
// dev so an operator can tell exactly what is running.
var (
	version = "dev"
	commit  = "unknown"
	builtAt = "unknown"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Printf("realtime-kit %s (commit %s, built %s)\n", version, commit, builtAt)
		return
	}
	if err := run(); err != nil {
		slog.Error("realtime-kit exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	level := slog.LevelInfo
	if cfg.Debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	server := api.New(livekit.New(cfg.UpstreamTimeout), api.Options{
		Token:          cfg.Token,
		MaxRequestSize: cfg.MaxRequestSize,
		Logger:         logger,
	})

	httpServer := &http.Server{
		Addr:         cfg.Addr,
		Handler:      server.Handler(),
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}

	// SIGINT/SIGTERM stops accepting and lets in-flight joins finish: a request
	// cut off here is a user who watched a call fail to connect.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		logger.Info("shutting down", "grace", cfg.ShutdownGrace.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}
