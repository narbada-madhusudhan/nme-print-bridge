package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// ─── Config ────────────────────────────────────────────────────────────────

type Config struct {
	HotelID             string `json:"hotel_id"`
	CertURL             string `json:"cert_url"`
	AdminAPIURL         string `json:"admin_api_url"`
	RestaurantBranchID  string `json:"restaurant_branch_id"`
	ServiceKey          string `json:"service_key,omitempty"`
	PollEnabled         bool   `json:"poll_enabled"`
	PollIntervalSeconds int    `json:"poll_interval_seconds"`
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
	os.MkdirAll(configDir(), 0755)
	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(configPath(), data, 0600)
}
