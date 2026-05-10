package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/serkostrov/2b-empty-call/internal/config"
	"github.com/serkostrov/2b-empty-call/internal/httpapi"
	"github.com/serkostrov/2b-empty-call/internal/logger"
	"github.com/serkostrov/2b-empty-call/internal/providers/gigachat"
	"github.com/serkostrov/2b-empty-call/internal/providers/salute"
	"github.com/serkostrov/2b-empty-call/internal/service"
	"github.com/serkostrov/2b-empty-call/internal/supabase"
	"github.com/serkostrov/2b-empty-call/internal/worker"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	log := logger.New(cfg.App.Level).With("app", cfg.App.Name, "env", cfg.App.Env, "version", cfg.App.Version)
	slog.SetDefault(log)

	httpClient := &http.Client{
		Timeout: 2 * time.Minute,
		Transport: &http.Transport{
			TLSClientConfig:       cfg.TLSConfig(),
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		},
	}

	saluteClient := salute.New(cfg.Sber, httpClient, log)
	gigachatClient := gigachat.New(cfg.Sber, httpClient)
	processor := service.NewProcessor(saluteClient, gigachatClient, cfg.Worker.Concurrency)
	supabaseClient := supabase.New(cfg.Supabase, httpClient, log)
	w := worker.New(cfg, supabaseClient, processor, log)

	h := httpapi.NewHandler(cfg, log)
	srv := &http.Server{
		Addr:         cfg.HTTP.Addr,
		Handler:      h.Routes(),
		ReadTimeout:  cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout,
		IdleTimeout:  cfg.HTTP.IdleTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("http server started", "addr", cfg.HTTP.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "error", err)
			stop()
		}
	}()

	go func() {
		if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("worker failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutdown started")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "error", err)
		_ = srv.Close()
	}
	log.Info("shutdown completed")
}
