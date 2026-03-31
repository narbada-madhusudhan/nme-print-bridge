package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_Defaults(t *testing.T) {
	// Use a temp dir to avoid touching real config
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	cfg := loadConfig()
	if cfg.CertURL != DefaultCertURL {
		t.Errorf("CertURL = %q, want %q", cfg.CertURL, DefaultCertURL)
	}
	if cfg.PollIntervalSeconds != DefaultPollInterval {
		t.Errorf("PollIntervalSeconds = %d, want %d", cfg.PollIntervalSeconds, DefaultPollInterval)
	}
	if cfg.PollEnabled {
		t.Error("PollEnabled should default to false")
	}
}

func TestLoadConfig_FromFile(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	cfgDir := filepath.Join(tmpDir, ConfigDirName)
	os.MkdirAll(cfgDir, 0755)

	cfg := Config{
		HotelID:             "test-hotel",
		CertURL:             "https://custom.cert.url",
		AdminAPIURL:         "https://admin.test.com",
		ServiceKey:          "my-key",
		PollEnabled:         true,
		PollIntervalSeconds: 10,
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(filepath.Join(cfgDir, ConfigFile), data, 0600)

	loaded := loadConfig()
	if loaded.HotelID != "test-hotel" {
		t.Errorf("HotelID = %q", loaded.HotelID)
	}
	if loaded.AdminAPIURL != "https://admin.test.com" {
		t.Errorf("AdminAPIURL = %q", loaded.AdminAPIURL)
	}
	if loaded.ServiceKey != "my-key" {
		t.Errorf("ServiceKey = %q", loaded.ServiceKey)
	}
	if !loaded.PollEnabled {
		t.Error("PollEnabled should be true")
	}
	if loaded.PollIntervalSeconds != 10 {
		t.Errorf("PollIntervalSeconds = %d, want 10", loaded.PollIntervalSeconds)
	}
}

func TestLoadConfig_MalformedJSON(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	cfgDir := filepath.Join(tmpDir, ConfigDirName)
	os.MkdirAll(cfgDir, 0755)
	os.WriteFile(filepath.Join(cfgDir, ConfigFile), []byte(`{broken json`), 0600)

	// Should not panic, returns defaults
	cfg := loadConfig()
	if cfg.CertURL != DefaultCertURL {
		t.Errorf("should use default CertURL on malformed config")
	}
}

func TestLoadConfig_PollIntervalMinimum(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	cfgDir := filepath.Join(tmpDir, ConfigDirName)
	os.MkdirAll(cfgDir, 0755)

	// PollInterval of 1 is below MinPollInterval (2), should be reset to default
	cfg := Config{PollIntervalSeconds: 1}
	data, _ := json.Marshal(cfg)
	os.WriteFile(filepath.Join(cfgDir, ConfigFile), data, 0600)

	loaded := loadConfig()
	if loaded.PollIntervalSeconds != DefaultPollInterval {
		t.Errorf("PollIntervalSeconds = %d, should be reset to %d", loaded.PollIntervalSeconds, DefaultPollInterval)
	}
}

func TestSaveConfig_RoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	cfg := Config{
		HotelID:             "roundtrip-hotel",
		CertURL:             "https://cert.test",
		AdminAPIURL:         "https://admin.test",
		ServiceKey:          "secret-key",
		PollEnabled:         true,
		PollIntervalSeconds: 15,
	}
	saveConfig(cfg)

	loaded := loadConfig()
	if loaded.HotelID != cfg.HotelID {
		t.Errorf("HotelID = %q, want %q", loaded.HotelID, cfg.HotelID)
	}
	if loaded.AdminAPIURL != cfg.AdminAPIURL {
		t.Errorf("AdminAPIURL = %q", loaded.AdminAPIURL)
	}
	if loaded.ServiceKey != cfg.ServiceKey {
		t.Errorf("ServiceKey = %q", loaded.ServiceKey)
	}
}

func TestCompareSemver(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"v1.0.0", "v1.0.0", 0},
		{"v2.0.0", "v1.0.0", 1},    // positive = a > b
		{"v1.0.0", "v2.0.0", -1},   // negative = a < b
		{"v1.10.0", "v1.9.0", 1},   // semver not lexicographic
		{"v2.0.0", "v1.99.99", 1},
		{"v1.0.1", "v1.0.0", 1},
		{"dev", "v1.0.0", 0},       // dev never triggers update
		{"v1.0.0", "dev", 0},
	}

	for _, tt := range tests {
		got := compareSemver(tt.a, tt.b)
		// Check sign, not exact value
		if (tt.want > 0 && got <= 0) || (tt.want < 0 && got >= 0) || (tt.want == 0 && got != 0) {
			t.Errorf("compareSemver(%q, %q) = %d, want sign of %d", tt.a, tt.b, got, tt.want)
		}
	}
}
