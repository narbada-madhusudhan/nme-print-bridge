package main

import (
	"encoding/json"
	"net/http"
)

// ─── HTTP Types ────────────────────────────────────────────────────────────

type Response struct {
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
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

// ─── Middleware ─────────────────────────────────────────────────────────────

func limitBody(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodySize)
}

func corsMiddleware(cm *CertManager, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		if cm.IsOriginAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		// Chrome 104+ Private Network Access: public websites (HTTPS) accessing
		// localhost require this header. Set unconditionally on all responses —
		// safe because we only bind to 127.0.0.1.
		w.Header().Set("Access-Control-Allow-Private-Network", "true")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
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
