package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// ─── Config ────────────────────────────────────────────────────────────────

type Config struct {
	HotelID             string   `json:"hotel_id"`
	CertURL             string   `json:"cert_url"`
	AdminAPIURL         string   `json:"admin_api_url"`
	RestaurantBranchID  string   `json:"restaurant_branch_id"`
	ServiceKey          string   `json:"service_key,omitempty"`
	PollEnabled         bool     `json:"poll_enabled"`
	PollIntervalSeconds int      `json:"poll_interval_seconds"`
	AllowedOrigins      []string `json:"allowed_origins,omitempty"`
}

func configDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ConfigDirName)
}

func configPath() string {
	return filepath.Join(configDir(), ConfigFile)
}

func loadConfig() Config {
	cfg := Config{CertURL: DefaultCertURL}
	data, err := os.ReadFile(configPath())
	if err != nil {
		cfg.PollIntervalSeconds = DefaultPollInterval
		return cfg
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("[config] Warning: failed to parse config.json: %v", err)
	}
	if cfg.CertURL == "" {
		cfg.CertURL = DefaultCertURL
	}
	if cfg.PollIntervalSeconds < MinPollInterval {
		cfg.PollIntervalSeconds = DefaultPollInterval
	}
	return cfg
}

func saveConfig(cfg Config) {
	dir := configDir()
	os.MkdirAll(dir, 0700)
	// MkdirAll leaves an already-existing dir's mode untouched, so chmod
	// explicitly — this tightens dirs from older installs that predate 0700.
	os.Chmod(dir, 0700)
	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(configPath(), data, 0600)
}

// resolveServiceKey applies env-over-config precedence: $SERVICE_KEY, when
// set, always wins over whatever was loaded from config.json. This lets
// operators avoid ever persisting the key to disk. fileKey is returned
// unchanged when the env var is unset, preserving old config.json-only setups.
func resolveServiceKey(fileKey string) string {
	if envKey := os.Getenv("SERVICE_KEY"); envKey != "" {
		return envKey
	}
	return fileKey
}
