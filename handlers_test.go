package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ─── Status & Health ───────────────────────────────────────────────────────

func TestHandleStatus(t *testing.T) {
	w := httptest.NewRecorder()
	handleStatus(w, httptest.NewRequest("GET", "/", nil))

	if w.Code != 200 {
		t.Errorf("status code = %d, want 200", w.Code)
	}

	var body map[string]any
	json.NewDecoder(w.Body).Decode(&body)

	if body["name"] != AppName {
		t.Errorf("name = %v, want %s", body["name"], AppName)
	}
	if body["status"] != "running" {
		t.Errorf("status = %v, want running", body["status"])
	}
	if body["version"] == nil {
		t.Error("missing version")
	}
}

func TestHandleStatus_WithPoller(t *testing.T) {
	// Simulate active poller
	p := NewPoller(Config{AdminAPIURL: "http://test.local", PollIntervalSeconds: 5})
	p.jobsProcessed.Store(42)
	activePollerPtr.Store(p)
	defer activePollerPtr.Store(nil)

	w := httptest.NewRecorder()
	handleStatus(w, httptest.NewRequest("GET", "/", nil))

	var body map[string]any
	json.NewDecoder(w.Body).Decode(&body)

	poller, ok := body["poller"].(map[string]any)
	if !ok {
		t.Fatal("missing poller in status")
	}
	if poller["enabled"] != true {
		t.Error("poller should be enabled")
	}
	if poller["jobs_processed"] != float64(42) {
		t.Errorf("jobs_processed = %v, want 42", poller["jobs_processed"])
	}
	if poller["admin_api"] != "http://test.local" {
		t.Errorf("admin_api = %v", poller["admin_api"])
	}
}

func TestHandleHealth(t *testing.T) {
	w := httptest.NewRecorder()
	handleHealth(w, httptest.NewRequest("GET", "/health", nil))

	if w.Code != 200 {
		t.Errorf("status code = %d, want 200", w.Code)
	}

	var body Response
	json.NewDecoder(w.Body).Decode(&body)
	if !body.Success || body.Message != "ok" {
		t.Errorf("unexpected response: %+v", body)
	}
}

// ─── Network Print Handler ─────────────────────────────────────────────────

func TestHandlePrintNetwork_MissingIP(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/print/network", strings.NewReader(`{"port":9100,"raw":"test"}`))
	handlePrintNetwork(w, r)

	if w.Code != 400 {
		t.Errorf("status code = %d, want 400", w.Code)
	}
}

func TestHandlePrintNetwork_InvalidIP(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/print/network", strings.NewReader(`{"ip":"not-an-ip","raw":"test"}`))
	handlePrintNetwork(w, r)

	if w.Code != 400 {
		t.Errorf("status code = %d, want 400", w.Code)
	}
}

func TestHandlePrintNetwork_InvalidPort(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/print/network", strings.NewReader(`{"ip":"192.168.1.1","port":99999,"raw":"test"}`))
	handlePrintNetwork(w, r)

	if w.Code != 400 {
		t.Errorf("status code = %d, want 400", w.Code)
	}
}

func TestHandlePrintNetwork_NoData(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/print/network", strings.NewReader(`{"ip":"192.168.1.1"}`))
	handlePrintNetwork(w, r)

	if w.Code != 400 {
		t.Errorf("status code = %d, want 400", w.Code)
	}
}

func TestHandlePrintNetwork_InvalidJSON(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/print/network", strings.NewReader(`{broken`))
	handlePrintNetwork(w, r)

	if w.Code != 400 {
		t.Errorf("status code = %d, want 400", w.Code)
	}
}

func TestHandlePrintNetwork_InvalidBase64(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/print/network", strings.NewReader(`{"ip":"192.168.1.1","data":"not-base64!!!"}`))
	handlePrintNetwork(w, r)

	if w.Code != 400 {
		t.Errorf("status code = %d, want 400", w.Code)
	}
}

func TestHandlePrintNetwork_Success(t *testing.T) {
	// Start mock printer
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port
	received := make(chan []byte, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		data, _ := io.ReadAll(conn)
		received <- data
	}()

	body := fmt.Sprintf(`{"ip":"127.0.0.1","port":%d,"raw":"Hello Printer"}`, port)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/print/network", strings.NewReader(body))
	handlePrintNetwork(w, r)

	if w.Code != 200 {
		t.Errorf("status code = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	var resp Response
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.Success {
		t.Errorf("expected success, got error: %s", resp.Error)
	}
}

func TestHandlePrintNetwork_Base64Data(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port
	received := make(chan []byte, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		data, _ := io.ReadAll(conn)
		received <- data
	}()

	// Send ESC/POS commands as base64
	escposBytes := []byte("\x1B@\x1Ba\x01Hello\n\n\n\n\n\n\x1DV\x00")
	b64 := base64.StdEncoding.EncodeToString(escposBytes)
	body := fmt.Sprintf(`{"ip":"127.0.0.1","port":%d,"data":"%s"}`, port, b64)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/print/network", strings.NewReader(body))
	handlePrintNetwork(w, r)

	if w.Code != 200 {
		t.Fatalf("status code = %d, body: %s", w.Code, w.Body.String())
	}

	data := <-received
	if string(data) != string(escposBytes) {
		t.Errorf("received %v, want %v", data, escposBytes)
	}
}

func TestHandlePrintNetwork_DefaultPort(t *testing.T) {
	// Port 0 should default to 9100
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/print/network", strings.NewReader(`{"ip":"192.168.1.1","raw":"test"}`))
	handlePrintNetwork(w, r)

	// Will fail to connect (no printer on 9100) but should NOT return 400
	if w.Code == 400 {
		t.Error("port 0 should default to 9100, not fail validation")
	}
}

func TestHandlePrintNetwork_NegativePort(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/print/network", strings.NewReader(`{"ip":"192.168.1.1","port":-1,"raw":"test"}`))
	handlePrintNetwork(w, r)

	if w.Code != 400 {
		t.Errorf("negative port should fail validation, got %d", w.Code)
	}
}

// ─── Test Handler ──────────────────────────────────────────────────────────

func TestHandleTest_MissingIP(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader(`{"port":9100}`))
	handleTest(w, r)

	if w.Code != 400 {
		t.Errorf("status code = %d, want 400", w.Code)
	}
}

func TestHandleTest_InvalidIP(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader(`{"ip":"hostname"}`))
	handleTest(w, r)

	if w.Code != 400 {
		t.Errorf("status code = %d, want 400", w.Code)
	}
}

func TestHandleTest_Online(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port
	go func() {
		conn, _ := listener.Accept()
		if conn != nil {
			conn.Close()
		}
	}()

	body := fmt.Sprintf(`{"ip":"127.0.0.1","port":%d}`, port)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader(body))
	handleTest(w, r)

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["online"] != true {
		t.Errorf("expected online=true, got %v", resp)
	}
}

func TestHandleTest_Offline(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader(`{"ip":"127.0.0.1","port":59998}`))
	handleTest(w, r)

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["online"] != false {
		t.Errorf("expected online=false for closed port")
	}
}

// ─── CORS Middleware ───────────────────────────────────────────────────────

func TestCorsMiddleware_AllowedOrigin(t *testing.T) {
	cfg := Config{CertURL: DefaultCertURL}
	cm, _ := NewCertManager(cfg)

	handler := corsMiddleware(cm, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Origin", "http://localhost:3000")
	handler(w, r)

	if w.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Error("expected localhost:3000 to be allowed in dev mode")
	}
}

func TestCorsMiddleware_DisallowedOrigin(t *testing.T) {
	cfg := Config{CertURL: DefaultCertURL}
	cm, _ := NewCertManager(cfg)

	handler := corsMiddleware(cm, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Origin", "https://evil.com")
	handler(w, r)

	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("evil.com should not be allowed")
	}
}

func TestCorsMiddleware_Preflight(t *testing.T) {
	cfg := Config{CertURL: DefaultCertURL}
	cm, _ := NewCertManager(cfg)

	innerCalled := false
	handler := corsMiddleware(cm, func(w http.ResponseWriter, r *http.Request) {
		innerCalled = true
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("OPTIONS", "/", nil)
	r.Header.Set("Origin", "http://localhost:3000")
	handler(w, r)

	if w.Code != 200 {
		t.Errorf("preflight should return 200, got %d", w.Code)
	}
	if innerCalled {
		t.Error("inner handler should not be called for OPTIONS")
	}
}

func TestCorsMiddleware_PrivateNetwork(t *testing.T) {
	cfg := Config{CertURL: DefaultCertURL}
	cm, _ := NewCertManager(cfg)

	handler := corsMiddleware(cm, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("OPTIONS", "/", nil)
	r.Header.Set("Access-Control-Request-Private-Network", "true")
	handler(w, r)

	if w.Header().Get("Access-Control-Allow-Private-Network") != "true" {
		t.Error("expected Allow-Private-Network header")
	}
}

// ─── WriteJSON ─────────────────────────────────────────────────────────────

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, 201, Response{Success: true, Message: "created"})

	if w.Code != 201 {
		t.Errorf("status code = %d, want 201", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}

	var body Response
	json.NewDecoder(w.Body).Decode(&body)
	if !body.Success || body.Message != "created" {
		t.Errorf("unexpected body: %+v", body)
	}
}
