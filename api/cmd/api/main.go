package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/config"
	httpapi "github.com/nimbuslabs-dev/kamui-cloud/api/internal/http"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/registry"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/scheduler"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}

	reg := registry.NewRegistry()
	queue := scheduler.NewQueue(time.Now)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	engine, closeCore, err := openCore(ctx, cfg)
	if err != nil {
		slog.Error("initialize core", "error", err)
		os.Exit(1)
	}
	defer closeCore()
	router, err := httpapi.NewRouterWithCore(cfg, reg, queue, engine)
	if err != nil {
		fmt.Fprintf(os.Stderr, "initialize API: %v\n", err)
		os.Exit(1)
	}

	host := "127.0.0.1"
	if os.Getenv("KAMUI_CORE_EXECUTOR") == "manager" {
		host = ""
	}
	srv := &http.Server{
		Addr:              net.JoinHostPort(host, cfg.Port),
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      160 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	engineDone := make(chan struct{})
	go func() { defer close(engineDone); engine.Run(ctx) }()
	defer func() { stop(); <-engineDone }()

	serverErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		fmt.Fprintf(os.Stderr, "server error: %s\n", err)
		os.Exit(1)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "shutdown error: %s\n", err)
			os.Exit(1)
		}
	}
}
