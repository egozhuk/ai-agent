package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Mode string

const (
	ModePolling Mode = "polling"
	ModeWebhook Mode = "webhook"
)

type Config struct {
	Mode               Mode
	AccessMode         string
	StorePath          string
	MaxBotToken        string
	MaxAPIBaseURL      string
	MaxCACertPath      string
	MistralAPIKey      string
	MistralAPIBaseURL  string
	MistralModel       string
	SearchAPIBaseURL   string
	OwnerMaxUserID     int64
	WebhookAddr        string
	WebhookSecret      string
	PublicBaseURL      string
	TokenEncryptionKey string
	GoogleClientID     string
	GoogleClientSecret string
}

func Load() (Config, error) {
	cfg := Config{
		Mode:               Mode(env("BOT_MODE", string(ModePolling))),
		AccessMode:         env("ACCESS_MODE", "owner"),
		StorePath:          env("STORE_PATH", "data/agent.db"),
		MaxBotToken:        strings.TrimSpace(os.Getenv("MAX_BOT_TOKEN")),
		MaxAPIBaseURL:      env("MAX_API_BASE_URL", "https://platform-api2.max.ru"),
		MaxCACertPath:      strings.TrimSpace(os.Getenv("MAX_CA_CERT_PATH")),
		MistralAPIKey:      strings.TrimSpace(os.Getenv("MISTRAL_API_KEY")),
		MistralAPIBaseURL:  env("MISTRAL_API_BASE_URL", "https://api.mistral.ai"),
		MistralModel:       env("MISTRAL_MODEL", "mistral-small-latest"),
		SearchAPIBaseURL:   env("SEARCH_API_BASE_URL", "http://127.0.0.1:8888"),
		WebhookAddr:        env("WEBHOOK_ADDR", ":8080"),
		WebhookSecret:      strings.TrimSpace(os.Getenv("WEBHOOK_SECRET")),
		PublicBaseURL:      strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL")), "/"),
		TokenEncryptionKey: strings.TrimSpace(os.Getenv("TOKEN_ENCRYPTION_KEY")),
		GoogleClientID:     strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_ID")),
		GoogleClientSecret: strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_SECRET")),
	}

	owner, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("OWNER_MAX_USER_ID")), 10, 64)
	if err != nil || owner <= 0 {
		return Config{}, errors.New("OWNER_MAX_USER_ID must be set to your MAX user id")
	}
	cfg.OwnerMaxUserID = owner

	if cfg.MaxBotToken == "" {
		return Config{}, errors.New("MAX_BOT_TOKEN is required")
	}
	if cfg.MistralAPIKey == "" {
		return Config{}, errors.New("MISTRAL_API_KEY is required")
	}
	if cfg.Mode != ModePolling && cfg.Mode != ModeWebhook {
		return Config{}, fmt.Errorf("BOT_MODE must be %q or %q", ModePolling, ModeWebhook)
	}
	if cfg.AccessMode != "owner" && cfg.AccessMode != "all" {
		return Config{}, errors.New("ACCESS_MODE must be owner or all")
	}
	if cfg.Mode == ModeWebhook && cfg.WebhookSecret == "" {
		return Config{}, errors.New("WEBHOOK_SECRET is required in webhook mode")
	}
	if (cfg.GoogleClientID != "" || cfg.GoogleClientSecret != "") && (cfg.GoogleClientID == "" || cfg.GoogleClientSecret == "" || cfg.PublicBaseURL == "" || cfg.TokenEncryptionKey == "") {
		return Config{}, errors.New("GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET, PUBLIC_BASE_URL, and TOKEN_ENCRYPTION_KEY are all required to enable Google integrations")
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
