// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Command cystemd-server runs the CYSTEMD control plane (API, web UI and
// application controller) as a standalone process — typically a Nomad job
// with a Vault Agent sidecar (deploy/nomad/cystemd-server.nomad.hcl) — for
// clusters running upstream Nomad. The cystemd agent embeds the same control
// plane in its server agents.
//
// It links only MPL-2.0 HashiCorp API clients, never Nomad itself.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gitaginanjar/cystemd/internal/buildinfo"
	"github.com/gitaginanjar/cystemd/internal/config"
	"github.com/gitaginanjar/cystemd/internal/controlplane"
	"github.com/gitaginanjar/cystemd/internal/server"
	"github.com/gitaginanjar/cystemd/ui"
)

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", os.Getenv("CYSTEMD_SERVER_CONFIG"), "control plane configuration file (HCL)")
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("cystemd-server %s\ncommit: %s\nbuild_time: %s\n", buildinfo.Version(), buildinfo.Commit(), buildinfo.BuildTime())
		return 0
	}

	cfg := config.Default()
	if *configPath != "" {
		c, err := config.Load(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cystemd-server: %v\n", err)
			return 1
		}
		cfg = c
	}
	logger := buildinfo.Logger(cfg.LogLevel, cfg.LogJSON)
	logger.Info("cystemd-server starting", "version", buildinfo.Version(), "commit", buildinfo.Commit(),
		"nomad", cfg.Nomad.Address, "consul", cfg.ConsulEnabled(), "vault", cfg.VaultEnabled())
	if !ui.Built() {
		logger.Warn("this binary embeds no web UI build; run `make ui` before building")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cp, err := controlplane.New(ctx, cfg, controlplane.Options{
		Logger: logger,
		UI:     ui.FS(),
		Build: server.BuildInfo{
			Version: buildinfo.Version(), GitCommit: buildinfo.Commit(), BuildDate: buildinfo.BuildTime(), Mode: "standalone",
		},
	})
	if err != nil {
		logger.Error("cannot start the control plane", "error", err)
		return 1
	}
	defer cp.Close()

	srv := &http.Server{
		Addr:              cfg.BindAddr,
		Handler:           cp.Server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: streams (SSE) stay open.
		IdleTimeout: 2 * time.Minute,
	}
	if cfg.TLSCertFile != "" {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("API and UI listening", "addr", cfg.BindAddr, "tls", cfg.TLSCertFile != "")
		if cfg.TLSCertFile != "" {
			errCh <- srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			errCh <- srv.ListenAndServe()
		}
	}()
	go cp.Run(ctx)

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server stopped", "error", err)
			return 1
		}
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return 0
}
