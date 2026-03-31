// NME Print Bridge — Thermal Printer Service for Hotels
// Single binary, zero dependencies. Download and run.
//
// Connects web-based POS systems to thermal printers via HTTP.
// Uses certificate-based trust for multi-hotel security.
//
// Endpoints:
//   GET  /             Status + info
//   GET  /health       Health check
//   GET  /printers     List installed printers
//   POST /print/network  Print to network printer (TCP)
//   POST /print/usb      Print to USB/OS printer
//   POST /test           Test printer connectivity

package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Version is set at build time via: go build -ldflags "-X main.Version=vX.Y.Z"
var Version = "dev"

const Port = 9120

// Root public key — baked in at compile time via ldflags
// Override with: go build -ldflags "-X main.RootPublicKeyB64=..."
var RootPublicKeyB64 = "PYpHIvPZS5ynAaz2iUy0iD3FAiizQ1Wi0Ee7AUHb2Ho="

// Default cert URL — override via config or CLI flag
var DefaultCertURL = "https://printbridge.narbadatech.com/api/certs"

// Built-in allowed origins — always allowed regardless of certificate.
// These are baked in for production use before the central cert API is live.
// Remove once cert system is fully deployed.
var BuiltInAllowedOrigins = []string{
	"https://godawariresort.com",
	"http://godawariresort.com",
	"https://admin.godawariresort.com",
	"https://pos.godawariresort.com",
	"https://www.godawariresort.com",
}

// ─── Certificate Types ─────────────────────────────────────────────────────

type CertPayload struct {
	HotelID        string   `json:"hotel_id"`
	HotelName      string   `json:"hotel_name"`
	AllowedOrigins []string `json:"allowed_origins"`
	IssuedAt       string   `json:"issued_at"`
	ExpiresAt      string   `json:"expires_at"`
}

type SignedCert struct {
	Payload   CertPayload `json:"payload"`
	Signature string      `json:"signature"` // base64 ed25519 signature of JSON payload
}

// ─── Config ────────────────────────────────────────────────────────────────

type Config struct {
	HotelID string `json:"hotel_id"`
	CertURL string `json:"cert_url"`
}

func configDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".printbridge")
}

func configPath() string {
	return filepath.Join(configDir(), "config.json")
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
	return cfg
}

func saveConfig(cfg Config) {
	os.MkdirAll(configDir(), 0755)
	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(configPath(), data, 0600)
}

// ─── Certificate Manager ───────────────────────────────────────────────────

type CertManager struct {
	mu             sync.RWMutex
	cert           *SignedCert
	allowedOrigins map[string]bool
	rootPubKey     ed25519.PublicKey
	config         Config
	cachePath      string
}

func NewCertManager(cfg Config) (*CertManager, error) {
	pubKeyBytes, err := base64.StdEncoding.DecodeString(RootPublicKeyB64)
	if err != nil {
		return nil, fmt.Errorf("invalid root public key: %w", err)
	}

	cm := &CertManager{
		rootPubKey:     ed25519.PublicKey(pubKeyBytes),
		config:         cfg,
		allowedOrigins: make(map[string]bool),
		cachePath:      filepath.Join(configDir(), "cert-cache.json"),
	}

	// Always allow localhost for development
	cm.allowedOrigins["http://localhost:3000"] = true
	cm.allowedOrigins["http://localhost:3001"] = true
	cm.allowedOrigins["https://localhost:3000"] = true

	// Built-in production origins (hardcoded for this version)
	for _, origin := range BuiltInAllowedOrigins {
		cm.allowedOrigins[origin] = true
	}

	return cm, nil
}

func (cm *CertManager) IsOriginAllowed(origin string) bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.allowedOrigins[origin]
}

func (cm *CertManager) FetchAndVerify() error {
	if cm.config.HotelID == "" {
		return fmt.Errorf("no hotel_id configured")
	}

	url := fmt.Sprintf("%s/%s", cm.config.CertURL, cm.config.HotelID)
	log.Printf("[cert] Fetching certificate from %s", url)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		// Try loading cached cert
		return cm.loadCachedCert()
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("[cert] Server returned %d, trying cache", resp.StatusCode)
		return cm.loadCachedCert()
	}

	var apiResp struct {
		Success bool       `json:"success"`
		Data    SignedCert `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return cm.loadCachedCert()
	}

	if !apiResp.Success {
		return cm.loadCachedCert()
	}

	cert := &apiResp.Data
	if err := cm.verifyCert(cert); err != nil {
		return fmt.Errorf("certificate verification failed: %w", err)
	}

	cm.applyCert(cert)
	cm.cacheCert(cert)
	log.Printf("[cert] Certificate verified for %s (%s)", cert.Payload.HotelName, cert.Payload.HotelID)
	return nil
}

func (cm *CertManager) verifyCert(cert *SignedCert) error {
	// Serialize payload to JSON for signature verification
	payloadBytes, err := json.Marshal(cert.Payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	sigBytes, err := base64.StdEncoding.DecodeString(cert.Signature)
	if err != nil {
		return fmt.Errorf("invalid signature encoding: %w", err)
	}

	if !ed25519.Verify(cm.rootPubKey, payloadBytes, sigBytes) {
		return fmt.Errorf("signature verification failed")
	}

	// Check not-before
	if cert.Payload.IssuedAt != "" {
		issuedAt, err := time.Parse(time.RFC3339, cert.Payload.IssuedAt)
		if err != nil {
			return fmt.Errorf("invalid issued_at: %w", err)
		}
		if time.Now().Before(issuedAt) {
			return fmt.Errorf("certificate not yet valid (issued_at: %s)", cert.Payload.IssuedAt)
		}
	}

	// Check expiry
	expiresAt, err := time.Parse(time.RFC3339, cert.Payload.ExpiresAt)
	if err != nil {
		return fmt.Errorf("invalid expires_at: %w", err)
	}
	if time.Now().After(expiresAt) {
		return fmt.Errorf("certificate expired at %s", cert.Payload.ExpiresAt)
	}

	return nil
}

func (cm *CertManager) applyCert(cert *SignedCert) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	cm.cert = cert
	// Reset and rebuild allowed origins
	cm.allowedOrigins = map[string]bool{
		"http://localhost:3000":  true,
		"http://localhost:3001":  true,
		"https://localhost:3000": true,
	}
	// Built-in origins always allowed
	for _, origin := range BuiltInAllowedOrigins {
		cm.allowedOrigins[origin] = true
	}
	// Certificate origins
	for _, origin := range cert.Payload.AllowedOrigins {
		cm.allowedOrigins[origin] = true
	}
}

func (cm *CertManager) cacheCert(cert *SignedCert) {
	os.MkdirAll(configDir(), 0755)
	data, _ := json.MarshalIndent(cert, "", "  ")
	os.WriteFile(cm.cachePath, data, 0600)
}

func (cm *CertManager) loadCachedCert() error {
	data, err := os.ReadFile(cm.cachePath)
	if err != nil {
		return fmt.Errorf("no cached certificate available")
	}

	var cert SignedCert
	if err := json.Unmarshal(data, &cert); err != nil {
		return fmt.Errorf("invalid cached certificate")
	}

	if err := cm.verifyCert(&cert); err != nil {
		return fmt.Errorf("cached certificate invalid: %w", err)
	}

	cm.applyCert(&cert)
	log.Printf("[cert] Using cached certificate for %s", cert.Payload.HotelName)
	return nil
}

func (cm *CertManager) StartPeriodicRefresh() {
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			if err := cm.FetchAndVerify(); err != nil {
				log.Printf("[cert] Periodic refresh failed: %v", err)
			}
		}
	}()
}

// ─── HTTP Types ────────────────────────────────────────────────────────────

type Response struct {
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

type PrinterInfo struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type NetworkPrintReq struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
	Data string `json:"data"` // base64
	Raw  string `json:"raw"`  // plain text
}

type USBPrintReq struct {
	Printer string `json:"printer"`
	Data    string `json:"data"`
	Raw     string `json:"raw"`
}

type TestReq struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// ─── CORS Middleware ───────────────────────────────────────────────────────

func corsMiddleware(cm *CertManager, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		if cm.IsOriginAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		// Chrome 104+ Private Network Access: public websites accessing localhost
		// require this header in the preflight response. Without it, Chrome blocks
		// the request entirely. Safe because we only bind to 127.0.0.1.
		if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
		}

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		limitBody(w, r)
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ─── Handlers ──────────────────────────────────────────────────────────────

func handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{
		"name":     "NME Print Bridge",
		"version":  Version,
		"platform": runtime.GOOS,
		"arch":     runtime.GOARCH,
		"status":   "running",
	})
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, Response{Success: true, Message: "ok"})
}

func handleListPrinters(w http.ResponseWriter, _ *http.Request) {
	printers, err := listPrintersCached()
	if err != nil {
		writeJSON(w, 500, Response{Success: false, Error: err.Error()})
		return
	}
	writeJSON(w, 200, Response{Success: true, Data: printers})
}

func handlePrintNetwork(w http.ResponseWriter, r *http.Request) {
	var req NetworkPrintReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, Response{Success: false, Error: "Invalid JSON"})
		return
	}
	if req.IP == "" {
		writeJSON(w, 400, Response{Success: false, Error: "ip is required"})
		return
	}
	if err := validateIP(req.IP); err != nil {
		writeJSON(w, 400, Response{Success: false, Error: err.Error()})
		return
	}
	if req.Port == 0 {
		req.Port = 9100
	}
	if req.Port < 1 || req.Port > 65535 {
		writeJSON(w, 400, Response{Success: false, Error: "port must be 1-65535"})
		return
	}

	printData, err := decodeData(req.Data, req.Raw)
	if err != nil {
		writeJSON(w, 400, Response{Success: false, Error: err.Error()})
		return
	}
	if len(printData) == 0 {
		writeJSON(w, 400, Response{Success: false, Error: "No print data"})
		return
	}

	addr := net.JoinHostPort(req.IP, strconv.Itoa(req.Port))
	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		writeJSON(w, 500, Response{Success: false, Error: fmt.Sprintf("Connection failed: %s", err.Error())})
		return
	}
	defer conn.Close()

	conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if _, err = conn.Write(printData); err != nil {
		writeJSON(w, 500, Response{Success: false, Error: fmt.Sprintf("Write failed: %s", err.Error())})
		return
	}

	writeJSON(w, 200, Response{Success: true, Message: fmt.Sprintf("Sent %d bytes to %s", len(printData), addr)})
}

func handlePrintUSB(w http.ResponseWriter, r *http.Request) {
	var req USBPrintReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, Response{Success: false, Error: "Invalid JSON"})
		return
	}
	if req.Printer == "" {
		writeJSON(w, 400, Response{Success: false, Error: "printer name is required"})
		return
	}
	if err := validatePrinterName(req.Printer); err != nil {
		writeJSON(w, 400, Response{Success: false, Error: err.Error()})
		return
	}

	printData, err := decodeData(req.Data, req.Raw)
	if err != nil {
		writeJSON(w, 400, Response{Success: false, Error: err.Error()})
		return
	}
	if len(printData) == 0 {
		writeJSON(w, 400, Response{Success: false, Error: "No print data"})
		return
	}

	if err := printToUSB(req.Printer, printData); err != nil {
		writeJSON(w, 500, Response{Success: false, Error: err.Error()})
		return
	}

	writeJSON(w, 200, Response{Success: true, Message: fmt.Sprintf("Sent to %s", req.Printer)})
}

func handleTest(w http.ResponseWriter, r *http.Request) {
	var req TestReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, Response{Success: false, Error: "Invalid JSON"})
		return
	}
	if req.IP == "" {
		writeJSON(w, 400, Response{Success: false, Error: "ip is required"})
		return
	}
	if err := validateIP(req.IP); err != nil {
		writeJSON(w, 400, Response{Success: false, Error: err.Error()})
		return
	}
	if req.Port == 0 {
		req.Port = 9100
	}
	if req.Port < 1 || req.Port > 65535 {
		writeJSON(w, 400, Response{Success: false, Error: "port must be 1-65535"})
		return
	}

	addr := net.JoinHostPort(req.IP, strconv.Itoa(req.Port))
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		writeJSON(w, 200, map[string]any{"success": true, "online": false, "error": err.Error()})
		return
	}
	conn.Close()
	writeJSON(w, 200, map[string]any{"success": true, "online": true})
}

// ─── Helpers ───────────────────────────────────────────────────────────────

func decodeData(b64, raw string) ([]byte, error) {
	if b64 != "" {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 data: %w", err)
		}
		return data, nil
	}
	return []byte(raw), nil
}

const maxBodySize = 10 * 1024 * 1024 // 10 MB

func limitBody(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
}

func validateIP(ip string) error {
	if net.ParseIP(ip) == nil {
		return fmt.Errorf("invalid IP address: %s", ip)
	}
	return nil
}

var (
	printerCache     []PrinterInfo
	printerCacheTime time.Time
	printerCacheMu   sync.Mutex
	printerCacheTTL  = 30 * time.Second
)

func listPrintersCached() ([]PrinterInfo, error) {
	printerCacheMu.Lock()
	defer printerCacheMu.Unlock()
	if printerCache != nil && time.Since(printerCacheTime) < printerCacheTTL {
		return printerCache, nil
	}
	printers, err := listPrinters()
	if err != nil {
		return nil, err
	}
	printerCache = printers
	printerCacheTime = time.Now()
	return printers, nil
}

func validatePrinterName(name string) error {
	printers, err := listPrintersCached()
	if err != nil {
		return fmt.Errorf("cannot list printers: %w", err)
	}
	for _, p := range printers {
		if p.Name == name {
			return nil
		}
	}
	return fmt.Errorf("unknown printer: %s", name)
}

func compareSemver(a, b string) int {
	// Non-release builds never trigger updates
	if a == "dev" || b == "dev" {
		return 0
	}
	aParts := strings.Split(strings.TrimPrefix(a, "v"), ".")
	bParts := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < 3; i++ {
		var ai, bi int
		if i < len(aParts) {
			ai, _ = strconv.Atoi(aParts[i])
		}
		if i < len(bParts) {
			bi, _ = strconv.Atoi(bParts[i])
		}
		if ai != bi {
			return ai - bi
		}
	}
	return 0
}

func listPrinters() ([]PrinterInfo, error) {
	switch runtime.GOOS {
	case "darwin", "linux":
		return listPrintersUnix()
	case "windows":
		return listPrintersWindows()
	default:
		return nil, fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

// probeUSBPrinter sends DLE EOT status command to check if printer is responsive.
// Returns true if the printer responds within timeout.
func probeUSBPrinter(printerName string) bool {
	switch runtime.GOOS {
	case "darwin", "linux":
		statusCmd := []byte{0x10, 0x04, 0x01} // DLE EOT 1 = query printer status
		tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("probe-%d.raw", time.Now().UnixNano()))
		if err := os.WriteFile(tmpFile, statusCmd, 0644); err != nil {
			return false
		}
		defer os.Remove(tmpFile)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "lp", "-d", printerName, "-o", "raw", tmpFile)
		err := cmd.Run()
		if ctx.Err() == context.DeadlineExceeded {
			return false
		}
		return err == nil
	case "windows":
		return canOpenPrinter(printerName)
	}
	return false
}

func listPrintersUnix() ([]PrinterInfo, error) {
	// Use lpstat -p for printer list — each printer may span multiple lines
	out, err := exec.Command("lpstat", "-p").CombinedOutput()
	if err != nil {
		return []PrinterInfo{}, nil
	}

	// Send DLE EOT probe to USB printers to trigger CUPS offline detection
	// (CUPS only discovers offline state when a job is attempted)
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "printer ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 && !isNetworkPrinter(parts[1]) {
				go probeUSBPrinter(parts[1])
			}
		}
	}

	// Brief pause to let CUPS update status after probe
	time.Sleep(500 * time.Millisecond)

	// Re-read status after probe
	out, err = exec.Command("lpstat", "-p").CombinedOutput()
	if err != nil {
		return []PrinterInfo{}, nil
	}

	fullOutput := string(out)
	var printers []PrinterInfo
	for _, line := range strings.Split(fullOutput, "\n") {
		if strings.HasPrefix(line, "printer ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				name := parts[1]
				enabled := strings.Contains(line, "enabled") && !strings.Contains(line, "disabled")
				// Check for offline indicators in the lpstat output
				if strings.Contains(fullOutput, name) &&
					(strings.Contains(fullOutput, "offline") || strings.Contains(fullOutput, "not responding")) {
					// Check if the offline message is for THIS printer
					printerSection := extractPrinterSection(fullOutput, name)
					if strings.Contains(printerSection, "offline") || strings.Contains(printerSection, "not responding") {
						enabled = false
					}
				}
				printers = append(printers, PrinterInfo{Name: name, Enabled: enabled})
			}
		}
	}
	return printers, nil
}

// extractPrinterSection gets all lpstat output lines related to a specific printer
func extractPrinterSection(output, printerName string) string {
	var section strings.Builder
	inSection := false
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "printer "+printerName+" ") {
			inSection = true
		} else if strings.HasPrefix(line, "printer ") {
			inSection = false
		}
		if inSection {
			section.WriteString(line)
			section.WriteString("\n")
		}
	}
	return section.String()
}

func normalizePN(name string) string {
	return strings.NewReplacer(" ", "_", "-", "_").Replace(strings.ToLower(name))
}

func isNetworkPrinter(name string) bool {
	// Network printers typically start with IP-like patterns
	return strings.HasPrefix(name, "_") && strings.Contains(name, "_")
}

// getConnectedUSBPrinters returns currently connected USB printer names
// Uses lpinfo -v on macOS/Linux (lists physically available devices)
// Uses Get-Printer on Windows (PrinterStatus reflects actual state)
func getConnectedUSBPrinters() map[string]bool {
	devices := map[string]bool{}

	switch runtime.GOOS {
	case "darwin", "linux":
		// lpinfo -v lists currently available (physically connected) devices
		out, err := exec.Command("lpinfo", "-v").CombinedOutput()
		if err != nil {
			return devices
		}
		for _, line := range strings.Split(string(out), "\n") {
			// Lines look like: direct usb://Printer/POS-80C?serial=012345678AB
			if strings.Contains(line, "usb://") {
				// Extract printer name from USB URI
				parts := strings.SplitN(line, "usb://", 2)
				if len(parts) == 2 {
					uri := parts[1]
					// URI format: Manufacturer/Model?serial=...
					if idx := strings.Index(uri, "?"); idx > 0 {
						uri = uri[:idx]
					}
					// Store both raw and normalized
					devices[uri] = true
					devices[normalizePN(uri)] = true
					// Also store just the model part
					uriParts := strings.SplitN(uri, "/", 2)
					if len(uriParts) == 2 {
						devices[uriParts[1]] = true
						devices[normalizePN(uriParts[1])] = true
					}
				}
			}
		}
	}

	return devices
}

func listPrintersWindows() ([]PrinterInfo, error) {
	// PrinterStatus: 0=Normal/Other, 1=Paused, 2=Error, 3=Deleting, 4=PaperJam, 5=PaperOut, 6=ManualFeed, 7=PaperProblem, 8=Offline
	out, err := exec.Command("powershell", "-Command",
		`Get-Printer | Select-Object Name, PrinterStatus | ConvertTo-Json`).CombinedOutput()
	if err != nil {
		return []PrinterInfo{}, nil
	}
	var raw any
	if err := json.Unmarshal(out, &raw); err != nil {
		return []PrinterInfo{}, nil
	}
	var items []map[string]any
	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				items = append(items, m)
			}
		}
	case map[string]any:
		items = append(items, v)
	}
	var printers []PrinterInfo
	for _, item := range items {
		name, _ := item["Name"].(string)
		// PrinterStatus 0 = Normal, anything else = problem
		status, _ := item["PrinterStatus"].(float64)
		enabled := status == 0
		printers = append(printers, PrinterInfo{Name: name, Enabled: enabled})
	}
	return printers, nil
}

func printToUSB(printerName string, data []byte) error {
	switch runtime.GOOS {
	case "darwin", "linux":
		tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("print-%d.raw", time.Now().UnixNano()))
		if err := os.WriteFile(tmpFile, data, 0644); err != nil {
			return fmt.Errorf("failed to write temp file: %w", err)
		}
		defer os.Remove(tmpFile)
		cmd := exec.Command("lp", "-d", printerName, "-o", "raw", tmpFile)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("lp error: %s — %s", err, string(out))
		}
	case "windows":
		if err := sendRawToPrinter(printerName, data); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
	return nil
}

// ─── Self-Update ───────────────────────────────────────────────────────────

const GitHubRepo = "narbada-madhusudhan/nme-print-bridge"

type UpdateInfo struct {
	Available      bool   `json:"available"`
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version"`
	DownloadURL    string `json:"download_url,omitempty"`
	ReleaseURL     string `json:"release_url,omitempty"`
}

var (
	cachedUpdate     *UpdateInfo
	cachedUpdateTime time.Time
	updateMu         sync.Mutex
)

func getAssetSuffix() string {
	switch {
	case runtime.GOOS == "windows":
		return "windows-amd64.exe"
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return "mac-arm64"
	case runtime.GOOS == "darwin":
		return "mac-amd64"
	default:
		return "linux-amd64"
	}
}

func checkForUpdate() (*UpdateInfo, error) {
	updateMu.Lock()
	defer updateMu.Unlock()

	// Cache for 1 hour
	if cachedUpdate != nil && time.Since(cachedUpdateTime) < time.Hour {
		return cachedUpdate, nil
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", GitHubRepo))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, err
	}

	suffix := getAssetSuffix()

	info := &UpdateInfo{
		Available:      compareSemver(release.TagName, Version) > 0,
		CurrentVersion: Version,
		LatestVersion:  release.TagName,
		ReleaseURL:     release.HTMLURL,
	}

	for _, asset := range release.Assets {
		if strings.HasSuffix(asset.Name, suffix) {
			info.DownloadURL = asset.BrowserDownloadURL
			break
		}
	}

	cachedUpdate = info
	cachedUpdateTime = time.Now()
	return info, nil
}

func performUpdate(downloadURL string) error {
	// Validate download URL is from GitHub
	if !strings.HasPrefix(downloadURL, "https://github.com/") && !strings.HasPrefix(downloadURL, "https://objects.githubusercontent.com/") {
		return fmt.Errorf("untrusted download URL: %s", downloadURL)
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine executable path: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("cannot resolve executable path: %w", err)
	}

	log.Printf("[update] Downloading from %s", downloadURL)
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download returned %d", resp.StatusCode)
	}

	// Write to temp file next to current binary
	tmpPath := exePath + ".update"
	tmpFile, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}

	const maxDownloadSize = 100 * 1024 * 1024 // 100 MB
	if _, err := io.Copy(tmpFile, io.LimitReader(resp.Body, maxDownloadSize)); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("download write failed: %w", err)
	}
	tmpFile.Close()

	// Make executable
	if err := os.Chmod(tmpPath, 0755); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod failed: %w", err)
	}

	// Replace old binary
	backupPath := exePath + ".backup"
	os.Remove(backupPath) // clean old backup
	if err := os.Rename(exePath, backupPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("backup failed: %w", err)
	}
	if err := os.Rename(tmpPath, exePath); err != nil {
		// Restore backup
		os.Rename(backupPath, exePath)
		return fmt.Errorf("replace failed: %w", err)
	}
	os.Remove(backupPath)

	log.Printf("[update] Binary replaced successfully. Restarting...")

	// Restart: exec the new binary (replaces current process on Unix, starts new + exits on Windows)
	execErr := syscallExec(exePath)
	if execErr != nil {
		log.Printf("[update] Auto-restart failed: %v — please restart manually", execErr)
	}
	return nil
}

// syscallExec replaces the current process with a new one.
// Unix: uses syscall.Exec to replace in-place (PID preserved for service managers).
// Windows: starts a new process and exits (service manager restarts).
func syscallExec(path string) error {
	if runtime.GOOS == "windows" {
		// Use wscript + VBS to restart silently (no console window flash).
		// Write a temp .vbs that launches the new binary hidden, then exit.
		vbsPath := path + ".restart.vbs"
		vbs := fmt.Sprintf("WScript.Sleep 500\r\nCreateObject(\"Wscript.Shell\").Run \"\"\"%s\"\"\", 0, False\r\n", path)
		if err := os.WriteFile(vbsPath, []byte(vbs), 0644); err != nil {
			// Fallback to exec.Command if VBS write fails
			cmd := exec.Command(path)
			if err := cmd.Start(); err != nil {
				return fmt.Errorf("failed to start new process: %w", err)
			}
			os.Exit(0)
			return nil
		}
		exec.Command("wscript.exe", vbsPath).Start()
		os.Exit(0)
		return nil
	}
	// Unix: replace current process in-place
	return syscall.Exec(path, []string{path}, os.Environ())
}

// GET /update/check — check for updates
func handleUpdateCheck(w http.ResponseWriter, _ *http.Request) {
	info, err := checkForUpdate()
	if err != nil {
		writeJSON(w, 200, Response{Success: true, Data: &UpdateInfo{
			Available:      false,
			CurrentVersion: Version,
			LatestVersion:  Version,
		}})
		return
	}
	writeJSON(w, 200, Response{Success: true, Data: info})
}

// POST /update/apply — download and apply update
func handleUpdateApply(w http.ResponseWriter, _ *http.Request) {
	info, err := checkForUpdate()
	if err != nil || !info.Available || info.DownloadURL == "" {
		writeJSON(w, 400, Response{Success: false, Error: "No update available"})
		return
	}

	writeJSON(w, 200, Response{Success: true, Message: "Updating... NME Print Bridge will restart."})

	// Apply update in background (response already sent)
	go func() {
		time.Sleep(500 * time.Millisecond) // let response flush
		if err := performUpdate(info.DownloadURL); err != nil {
			log.Printf("[update] Failed: %v", err)
		}
	}()
}

// ─── Auto-Start Install/Uninstall ──────────────────────────────────────────

func installAutoStart() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine executable path: %w", err)
	}
	// Resolve symlinks
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("cannot resolve executable path: %w", err)
	}

	switch runtime.GOOS {
	case "darwin":
		return installMacLaunchAgent(exePath)
	case "windows":
		return installWindowsStartup(exePath)
	case "linux":
		return installLinuxSystemd(exePath)
	default:
		return fmt.Errorf("auto-start not supported on %s — run the binary manually", runtime.GOOS)
	}
}

func uninstallAutoStart() error {
	switch runtime.GOOS {
	case "darwin":
		return uninstallMacLaunchAgent()
	case "windows":
		return uninstallWindowsStartup()
	case "linux":
		return uninstallLinuxSystemd()
	default:
		return fmt.Errorf("auto-start not supported on %s", runtime.GOOS)
	}
}

func installMacLaunchAgent(exePath string) error {
	home, _ := os.UserHomeDir()
	plistDir := filepath.Join(home, "Library", "LaunchAgents")
	plistPath := filepath.Join(plistDir, "com.nme.print-bridge.plist")

	os.MkdirAll(plistDir, 0755)

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.nme.print-bridge</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/print-bridge.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/print-bridge.log</string>
</dict>
</plist>`, exePath)

	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		return err
	}

	exec.Command("launchctl", "unload", plistPath).Run()
	if err := exec.Command("launchctl", "load", plistPath).Run(); err != nil {
		return fmt.Errorf("launchctl load failed: %w", err)
	}

	fmt.Printf("  ✓ Auto-start installed (macOS LaunchAgent)\n")
	fmt.Printf("  ✓ NME Print Bridge will start automatically on login\n")
	fmt.Printf("  ✓ Logs: /tmp/print-bridge.log\n")
	fmt.Printf("  ✓ Uninstall: %s --uninstall\n", exePath)
	return nil
}

func uninstallMacLaunchAgent() error {
	home, _ := os.UserHomeDir()
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.nme.print-bridge.plist")
	exec.Command("launchctl", "unload", plistPath).Run()
	os.Remove(plistPath)
	fmt.Println("  ✓ Auto-start removed")
	return nil
}

func installWindowsStartup(exePath string) error {
	home, _ := os.UserHomeDir()
	startupDir := filepath.Join(home, "AppData", "Roaming", "Microsoft", "Windows", "Start Menu", "Programs", "Startup")

	// Remove old .bat if upgrading from previous version
	oldBat := filepath.Join(startupDir, "NME Print Bridge.bat")
	os.Remove(oldBat)

	// Use .vbs instead of .bat — runs the exe with zero visible windows
	vbsPath := filepath.Join(startupDir, "NME Print Bridge.vbs")
	vbs := fmt.Sprintf("CreateObject(\"Wscript.Shell\").Run \"\"\"%s\"\"\", 0, False\r\n", exePath)

	if err := os.WriteFile(vbsPath, []byte(vbs), 0644); err != nil {
		return fmt.Errorf("failed to create startup script: %w", err)
	}

	fmt.Printf("  ✓ Auto-start installed (Windows Startup folder)\n")
	fmt.Printf("  ✓ NME Print Bridge will start automatically on login\n")
	fmt.Printf("  ✓ Uninstall: %s --uninstall\n", exePath)
	return nil
}

// migrateWindowsStartup removes legacy .bat and ensures .vbs auto-start exists.
// Runs on every startup so auto-updates (which skip --install) also get migrated.
func migrateWindowsStartup() {
	home, _ := os.UserHomeDir()
	startupDir := filepath.Join(home, "AppData", "Roaming", "Microsoft", "Windows", "Start Menu", "Programs", "Startup")
	batPath := filepath.Join(startupDir, "NME Print Bridge.bat")
	vbsPath := filepath.Join(startupDir, "NME Print Bridge.vbs")

	// Clean up temp restart VBS from auto-update
	if exePath, err := os.Executable(); err == nil {
		os.Remove(exePath + ".restart.vbs")
	}

	// Remove legacy .bat if it exists
	if _, err := os.Stat(batPath); err == nil {
		os.Remove(batPath)
		log.Println("[migrate] Removed legacy .bat startup file")
	}

	// Ensure .vbs exists (may be missing after auto-update)
	if _, err := os.Stat(vbsPath); os.IsNotExist(err) {
		exePath, err := os.Executable()
		if err != nil {
			return
		}
		exePath, _ = filepath.EvalSymlinks(exePath)
		vbs := fmt.Sprintf("CreateObject(\"Wscript.Shell\").Run \"\"\"%s\"\"\", 0, False\r\n", exePath)
		if err := os.WriteFile(vbsPath, []byte(vbs), 0644); err == nil {
			log.Println("[migrate] Created .vbs startup file")
		}
	}
}

func uninstallWindowsStartup() error {
	home, _ := os.UserHomeDir()
	startupDir := filepath.Join(home, "AppData", "Roaming", "Microsoft", "Windows", "Start Menu", "Programs", "Startup")
	os.Remove(filepath.Join(startupDir, "NME Print Bridge.vbs"))
	os.Remove(filepath.Join(startupDir, "NME Print Bridge.bat")) // clean up old format
	fmt.Println("  ✓ Auto-start removed")
	return nil
}

func installLinuxSystemd(exePath string) error {
	home, _ := os.UserHomeDir()
	serviceDir := filepath.Join(home, ".config", "systemd", "user")
	servicePath := filepath.Join(serviceDir, "print-bridge.service")

	os.MkdirAll(serviceDir, 0755)

	service := fmt.Sprintf(`[Unit]
Description=NME Print Bridge — Thermal Printer Service
After=network.target

[Service]
ExecStart=%s
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`, exePath)

	if err := os.WriteFile(servicePath, []byte(service), 0644); err != nil {
		return err
	}

	exec.Command("systemctl", "--user", "daemon-reload").Run()
	exec.Command("systemctl", "--user", "enable", "print-bridge").Run()
	if err := exec.Command("systemctl", "--user", "start", "print-bridge").Run(); err != nil {
		return fmt.Errorf("systemctl start failed: %w", err)
	}

	fmt.Printf("  ✓ Auto-start installed (systemd user service)\n")
	fmt.Printf("  ✓ NME Print Bridge will start automatically on login\n")
	fmt.Printf("  ✓ Uninstall: %s --uninstall\n", exePath)
	return nil
}

func uninstallLinuxSystemd() error {
	exec.Command("systemctl", "--user", "stop", "print-bridge").Run()
	exec.Command("systemctl", "--user", "disable", "print-bridge").Run()
	home, _ := os.UserHomeDir()
	os.Remove(filepath.Join(home, ".config", "systemd", "user", "print-bridge.service"))
	exec.Command("systemctl", "--user", "daemon-reload").Run()
	fmt.Println("  ✓ Auto-start removed")
	return nil
}

// ─── Main ──────────────────────────────────────────────────────────────────

func main() {
	// On Windows (-H windowsgui), stdout/stderr go nowhere.
	// Write logs to a file so errors are diagnosable.
	if runtime.GOOS == "windows" {
		os.MkdirAll(configDir(), 0755)
		logPath := filepath.Join(configDir(), "bridge.log")
		if logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
			log.SetOutput(logFile)
			// Also redirect fmt.Print/Println to log file via os.Stdout
			os.Stdout = logFile
			os.Stderr = logFile
		}
	}

	hotelID := flag.String("hotel-id", "", "Hotel ID for certificate lookup")
	certURL := flag.String("cert-url", "", "Certificate API URL")
	install := flag.Bool("install", false, "Install auto-start (runs on login)")
	uninstall := flag.Bool("uninstall", false, "Remove auto-start")
	flag.Parse()

	// Handle install/uninstall
	if *install {
		if err := installAutoStart(); err != nil {
			fmt.Fprintf(os.Stderr, "  ✗ Install failed: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if *uninstall {
		if err := uninstallAutoStart(); err != nil {
			fmt.Fprintf(os.Stderr, "  ✗ Uninstall failed: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Migrate: clean up legacy .bat startup file and ensure .vbs exists
	if runtime.GOOS == "windows" {
		migrateWindowsStartup()
	}

	// Load or create config
	cfg := loadConfig()
	if *hotelID != "" {
		cfg.HotelID = *hotelID
	}
	if *certURL != "" {
		cfg.CertURL = *certURL
	}
	saveConfig(cfg)

	// Initialize cert manager
	cm, err := NewCertManager(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize: %v", err)
	}

	// Fetch and verify certificate
	if cfg.HotelID != "" {
		if err := cm.FetchAndVerify(); err != nil {
			log.Printf("[cert] Warning: %v (only localhost connections allowed)", err)
		}
		cm.StartPeriodicRefresh()
	} else {
		log.Println("[cert] No hotel_id configured — only localhost connections allowed")
		log.Printf("[cert] Configure: %s --hotel-id YOUR_HOTEL_ID", os.Args[0])
	}

	// Routes
	mux := http.NewServeMux()
	mux.HandleFunc("/", corsMiddleware(cm, handleStatus))
	mux.HandleFunc("/health", corsMiddleware(cm, handleHealth))
	mux.HandleFunc("/printers", corsMiddleware(cm, handleListPrinters))
	mux.HandleFunc("/print/network", corsMiddleware(cm, handlePrintNetwork))
	mux.HandleFunc("/print/usb", corsMiddleware(cm, handlePrintUSB))
	mux.HandleFunc("/test", corsMiddleware(cm, handleTest))
	mux.HandleFunc("/update/check", corsMiddleware(cm, handleUpdateCheck))
	mux.HandleFunc("/update/apply", corsMiddleware(cm, handleUpdateApply))

	addr := fmt.Sprintf("127.0.0.1:%d", Port)

	fmt.Println()
	fmt.Println("  ╔═══════════════════════════════════════╗")
	fmt.Printf("  ║   NME Print Bridge v%-22s║\n", Version)
	fmt.Printf("  ║   http://%-28s║\n", addr)
	fmt.Println("  ╠═══════════════════════════════════════╣")
	fmt.Println("  ║  GET  /              Status            ║")
	fmt.Println("  ║  GET  /printers      List printers     ║")
	fmt.Println("  ║  POST /print/network Network printer   ║")
	fmt.Println("  ║  POST /print/usb     USB printer       ║")
	fmt.Println("  ║  POST /test          Test connection    ║")
	fmt.Println("  ║  GET  /update/check  Check for updates ║")
	fmt.Println("  ║  POST /update/apply  Apply update      ║")
	fmt.Println("  ╚═══════════════════════════════════════╝")
	if cfg.HotelID != "" {
		fmt.Printf("  Hotel: %s\n", cfg.HotelID)
	}

	// Auto-update on startup: check and apply silently
	go func() {
		// Wait for server to start before checking (so health checks pass during the window)
		time.Sleep(3 * time.Second)
		info, err := checkForUpdate()
		if err != nil || !info.Available {
			return
		}
		if info.DownloadURL == "" {
			log.Printf("[update] Update %s available but no download URL for this platform", info.LatestVersion)
			return
		}
		log.Printf("[update] Auto-updating: %s → %s", info.CurrentVersion, info.LatestVersion)
		if err := performUpdate(info.DownloadURL); err != nil {
			log.Printf("[update] Auto-update failed: %v", err)
		}
	}()

	srv := &http.Server{
		Addr:           addr,
		Handler:        mux,
		ReadTimeout:    15 * time.Second,
		WriteTimeout:   60 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1 MB
	}

	// Graceful shutdown on SIGINT/SIGTERM
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		log.Println("[server] Shutting down gracefully...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	fmt.Println()
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Println("[server] Stopped.")
}
