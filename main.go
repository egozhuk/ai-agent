package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"AI-agent/internal/agent"
	"AI-agent/internal/automation"
	"AI-agent/internal/config"
	"AI-agent/internal/integrations"
	"AI-agent/internal/maxapi"
	"AI-agent/internal/mistral"
	"AI-agent/internal/search"
	"AI-agent/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(1)
	}

	st, err := store.Open(cfg.StorePath)
	if err != nil {
		logger.Error("store init failed", "error", err)
		os.Exit(1)
	}

	maxHTTPClient, err := maxHTTPClient(cfg.MaxCACertPath)
	if err != nil {
		logger.Error("MAX HTTP client init failed", "error", err)
		os.Exit(1)
	}
	maxClient := maxapi.NewClient(cfg.MaxAPIBaseURL, cfg.MaxBotToken, maxHTTPClient)
	mistralClient := mistral.NewClient(cfg.MistralAPIBaseURL, cfg.MistralAPIKey, cfg.MistralModel, http.DefaultClient)
	searchClient := search.NewClient(cfg.SearchAPIBaseURL, http.DefaultClient)

	tools, err := integrations.NewRegistry(integrations.Options{
		GoogleClientID:     cfg.GoogleClientID,
		GoogleClientSecret: cfg.GoogleClientSecret,
		PublicBaseURL:      cfg.PublicBaseURL,
		TokenEncryptionKey: cfg.TokenEncryptionKey,
		Store:              st,
		HTTPClient:         http.DefaultClient,
	})
	if err != nil {
		logger.Error("integrations init failed", "error", err)
		os.Exit(1)
	}

	assistant := agent.New(agent.Options{
		OwnerUserID: cfg.OwnerMaxUserID,
		AccessMode:  cfg.AccessMode,
		Store:       st,
		Max:         maxClient,
		Mistral:     mistralClient,
		Search:      searchClient,
		Tools:       tools,
		Logger:      logger,
	})
	automationRunner := automation.NewRunner(st, maxClient, mistralClient, searchClient, logger)
	go automationRunner.Run(ctx)

	switch cfg.Mode {
	case config.ModePolling:
		if err := runOAuthServer(ctx, logger, cfg, tools); err != nil {
			logger.Error("oauth server failed", "error", err)
			os.Exit(1)
		}
		if err := runPolling(ctx, logger, maxClient, assistant); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("polling stopped", "error", err)
			os.Exit(1)
		}
	case config.ModeWebhook:
		if err := runWebhook(ctx, logger, cfg, assistant, tools); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("webhook stopped", "error", err)
			os.Exit(1)
		}
	default:
		logger.Error("unsupported mode", "mode", cfg.Mode)
		os.Exit(1)
	}
}

func maxHTTPClient(caCertPath string) (*http.Client, error) {
	if caCertPath == "" {
		return http.DefaultClient, nil
	}

	certPool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system cert pool: %w", err)
	}
	if certPool == nil {
		certPool = x509.NewCertPool()
	}

	for _, path := range filepath.SplitList(caCertPath) {
		if path == "" {
			continue
		}
		if err := appendCertFile(certPool, path); err != nil {
			return nil, err
		}
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: certPool}

	return &http.Client{Transport: transport}, nil
}

func appendCertFile(certPool *x509.CertPool, path string) error {
	certData, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read MAX CA cert %q: %w", path, err)
	}
	if certPool.AppendCertsFromPEM(certData) {
		return nil
	}
	cert, err := x509.ParseCertificate(certData)
	if err != nil {
		return fmt.Errorf("MAX CA cert %q is not valid PEM or DER: %w", path, err)
	}
	certPool.AddCert(cert)
	return nil
}

func runPolling(ctx context.Context, logger *slog.Logger, maxClient *maxapi.Client, assistant *agent.Agent) error {
	logger.Info("starting MAX long polling")
	var marker *int64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		resp, err := maxClient.GetUpdates(ctx, marker, []string{"message_created", "bot_started"})
		if err != nil {
			logger.Warn("get updates failed", "error", err)
			time.Sleep(3 * time.Second)
			continue
		}
		if resp.Marker != nil {
			marker = resp.Marker
		}
		for _, update := range resp.Updates {
			if err := assistant.HandleUpdate(ctx, update); err != nil {
				logger.Warn("update handling failed", "type", update.UpdateType, "error", err)
			}
		}
	}
}

func runOAuthServer(ctx context.Context, logger *slog.Logger, cfg config.Config, tools *integrations.Registry) error {
	if !tools.GoogleConfigured() {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/google/callback", tools.HandleGoogleCallback)
	server := &http.Server{
		Addr:              cfg.WebhookAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Info("starting OAuth callback server", "addr", cfg.WebhookAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("oauth callback server stopped", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Warn("oauth callback shutdown failed", "error", err)
		}
	}()
	return nil
}

func runWebhook(ctx context.Context, logger *slog.Logger, cfg config.Config, assistant *agent.Agent, tools *integrations.Registry) error {
	logger.Info("starting webhook server", "addr", cfg.WebhookAddr)
	mux := http.NewServeMux()
	mux.Handle("/webhook", maxapi.WebhookHandler(cfg.WebhookSecret, assistant.HandleUpdate))
	if tools.GoogleConfigured() {
		mux.HandleFunc("/oauth/google/callback", tools.HandleGoogleCallback)
	}
	server := &http.Server{
		Addr:              cfg.WebhookAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
