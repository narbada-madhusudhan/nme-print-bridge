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
	// AutoUpdateEnabled gates automatic self-update on startup. Defaults to
	// false (off): the bridge only logs "update available" and never
	// replaces its own binary unless this is explicitly set true.
	AutoUpdateEnabled bool `json:"auto_update_enabled"`
}

func configDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ConfigDirName)
}

func configPath() string {
	return filepath.Join(configDir(), ConfigFile)
}

func loadConfig() Config {
	cfg := Config{CertURL: DefaultCertURL, PollIntervalSeconds: DefaultPollInterval}
	if data, err := os.ReadFile(configPath()); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			log.Printf("[config] Warning: failed to parse config.json: %v", err)
		}
	}
	if cfg.CertURL == "" {
		cfg.CertURL = DefaultCertURL
	}
	if cfg.PollIntervalSeconds < MinPollInterval {
		cfg.PollIntervalSeconds = DefaultPollInterval
	}
	// DefaultAllowedOrigins is an always-present floor (owner decision, M6):
	// config.json can ADD origins but never DROP the built-ins, so union
	// them in every load rather than only seeding them on first run.
	for _, origin := range DefaultAllowedOrigins {
		cfg.AllowedOrigins = addUnique(cfg.AllowedOrigins, origin)
	}
	return cfg
}

func saveConfig(cfg Config) {
	dir := configDir()
	os.MkdirAll(dir, 0700)
	// MkdirAll leaves an already-existing dir's mode untouched, so chmod
	// explicitly — this tightens dirs from older installs that predate 0700.
	// Note: Unix perm bits are a no-op on Windows (resort front-desk PCs) —
	// real lockdown there needs an ACL; TODO if that's ever in scope.
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
