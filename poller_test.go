package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// ─── Poller Claim ──────────────────────────────────────────────────────────

func TestPoller_ClaimJobs_Success(t *testing.T) {
	pName := "TestPrinter"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/bridge/print-jobs/claim" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("X-Bridge-Key") != "test-key" {
			t.Error("missing bridge key header")
		}
		json.NewEncoder(w).Encode(claimResponse{
			Success: true,
			Data: []claimedJob{
				{
					ID:        "job-1",
					Content:   json.RawMessage(`{"type":"bill","text":"Hello"}`),
					CreatedAt: time.Now().Format(time.RFC3339),
					Printer:   &jobPrinter{ID: "p1", IPAddress: "192.168.1.100", Port: 9100, PrinterName: &pName},
				},
			},
		})
	}))
	defer server.Close()

	p := NewPoller(Config{AdminAPIURL: server.URL, ServiceKey: "test-key", PollIntervalSeconds: 5})
	jobs, err := p.claimJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].ID != "job-1" {
		t.Errorf("job ID = %q, want job-1", jobs[0].ID)
	}
	if *jobs[0].Printer.PrinterName != "TestPrinter" {
		t.Errorf("printer name = %v", jobs[0].Printer.PrinterName)
	}
}

func TestPoller_ClaimJobs_Empty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(claimResponse{Success: true, Data: []claimedJob{}})
	}))
	defer server.Close()

	p := NewPoller(Config{AdminAPIURL: server.URL, PollIntervalSeconds: 5})
	jobs, err := p.claimJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Errorf("expected 0 jobs, got %d", len(jobs))
	}
}

func TestPoller_ClaimJobs_Unauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer server.Close()

	p := NewPoller(Config{AdminAPIURL: server.URL, ServiceKey: "wrong-key", PollIntervalSeconds: 5})
	_, err := p.claimJobs()
	if err == nil {
		t.Error("expected error for 401")
	}
	if err.Error() != "unauthorized — check service_key in config" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPoller_ClaimJobs_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer server.Close()

	p := NewPoller(Config{AdminAPIURL: server.URL, PollIntervalSeconds: 5})
	_, err := p.claimJobs()
	if err == nil {
		t.Error("expected error for 500")
	}
}

func TestPoller_ClaimJobs_NetworkError(t *testing.T) {
	p := NewPoller(Config{AdminAPIURL: "http://localhost:1", PollIntervalSeconds: 5})
	_, err := p.claimJobs()
	if err == nil {
		t.Error("expected network error")
	}
}

func TestPoller_ClaimJobs_APIFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(claimResponse{
			Success: false,
			Error:   &struct {
				Message string `json:"message"`
			}{Message: "database error"},
		})
	}))
	defer server.Close()

	p := NewPoller(Config{AdminAPIURL: server.URL, PollIntervalSeconds: 5})
	_, err := p.claimJobs()
	if err == nil || err.Error() != "claim failed: database error" {
		t.Errorf("unexpected error: %v", err)
	}
}

// ─── Poller Status Update ──────────────────────────────────────────────────

func TestPoller_UpdateStatus(t *testing.T) {
	var receivedStatus, receivedError string
	var receivedJobID string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := splitPath(r.URL.Path)
		if len(parts) >= 4 {
			receivedJobID = parts[3]
		}
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		json.Unmarshal(body, &payload)
		receivedStatus = payload["status"]
		receivedError = payload["error_message"]
		w.WriteHeader(200)
	}))
	defer server.Close()

	p := NewPoller(Config{AdminAPIURL: server.URL, ServiceKey: "key", PollIntervalSeconds: 5})
	p.updateStatus("job-42", JobStatusFailed, "paper jam")

	if receivedJobID != "job-42" {
		t.Errorf("job ID = %q, want job-42", receivedJobID)
	}
	if receivedStatus != JobStatusFailed {
		t.Errorf("status = %q, want FAILED", receivedStatus)
	}
	if receivedError != "paper jam" {
		t.Errorf("error = %q, want 'paper jam'", receivedError)
	}
}

// ─── Poller Process Job — End-to-End Print Simulation ──────────────────────

func TestPoller_ProcessJob_NetworkPrint(t *testing.T) {
	// Mock printer (TCP)
	printerListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer printerListener.Close()
	printerPort := printerListener.Addr().(*net.TCPAddr).Port

	printReceived := make(chan []byte, 1)
	go func() {
		conn, err := printerListener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		data, _ := io.ReadAll(conn)
		printReceived <- data
	}()

	// Mock admin API for status update
	var statusUpdate string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		json.Unmarshal(body, &payload)
		statusUpdate = payload["status"]
		w.WriteHeader(200)
	}))
	defer apiServer.Close()

	p := NewPoller(Config{AdminAPIURL: apiServer.URL, PollIntervalSeconds: 5})
	reachable := map[string]bool{}

	job := claimedJob{
		ID:        "job-net-1",
		Content:   json.RawMessage(`{"type":"bill","text":"Receipt line 1\nReceipt line 2"}`),
		CreatedAt: time.Now().Format(time.RFC3339),
		Printer:   &jobPrinter{ID: "p1", IPAddress: "127.0.0.1", Port: printerPort},
	}

	p.processJob(job, reachable)

	// Verify print data arrived at mock printer
	select {
	case data := <-printReceived:
		if len(data) == 0 {
			t.Error("no data received by printer")
		}
		if !containsStr(string(data), "Receipt line 1") {
			t.Error("expected receipt content in print data")
		}
		t.Logf("Mock printer received %d bytes", len(data))
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for print data")
	}

	// Verify status was updated to COMPLETED
	time.Sleep(100 * time.Millisecond) // let async status update complete
	if statusUpdate != JobStatusCompleted {
		t.Errorf("status = %q, want COMPLETED", statusUpdate)
	}

	if p.jobsProcessed.Load() != 1 {
		t.Errorf("jobsProcessed = %d, want 1", p.jobsProcessed.Load())
	}
}

func TestPoller_ProcessJob_UnreachablePrinter(t *testing.T) {
	var statusUpdate string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		json.Unmarshal(body, &payload)
		statusUpdate = payload["status"]
		w.WriteHeader(200)
	}))
	defer apiServer.Close()

	p := NewPoller(Config{AdminAPIURL: apiServer.URL, PollIntervalSeconds: 5})
	pName := "UnknownPrinter"

	// Printer not in reachable set, job is recent → release to PENDING
	job := claimedJob{
		ID:        "job-unreach-1",
		Content:   json.RawMessage(`{"text":"test"}`),
		CreatedAt: time.Now().Format(time.RFC3339),
		Printer:   &jobPrinter{PrinterName: &pName},
	}

	reachable := map[string]bool{"KitchenPrinter": true}
	p.processJob(job, reachable)

	time.Sleep(100 * time.Millisecond)
	if statusUpdate != JobStatusPending {
		t.Errorf("recent unreachable job should be PENDING, got %q", statusUpdate)
	}
}

func TestPoller_ProcessJob_UnreachableTimeout(t *testing.T) {
	var statusUpdate string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		json.Unmarshal(body, &payload)
		statusUpdate = payload["status"]
		w.WriteHeader(200)
	}))
	defer apiServer.Close()

	p := NewPoller(Config{AdminAPIURL: apiServer.URL, PollIntervalSeconds: 5})
	pName := "OldPrinter"

	// Job older than UnreachableTimeoutSec → mark UNREACHABLE
	oldTime := time.Now().Add(-3 * time.Minute).Format(time.RFC3339)
	job := claimedJob{
		ID:        "job-old-1",
		Content:   json.RawMessage(`{"text":"test"}`),
		CreatedAt: oldTime,
		Printer:   &jobPrinter{PrinterName: &pName},
	}

	reachable := map[string]bool{"OtherPrinter": true}
	p.processJob(job, reachable)

	time.Sleep(100 * time.Millisecond)
	if statusUpdate != JobStatusUnreachable {
		t.Errorf("old unreachable job should be UNREACHABLE, got %q", statusUpdate)
	}
}

func TestPoller_ProcessJob_NoPrinter(t *testing.T) {
	var statusUpdate string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		json.Unmarshal(body, &payload)
		statusUpdate = payload["status"]
		w.WriteHeader(200)
	}))
	defer apiServer.Close()

	p := NewPoller(Config{AdminAPIURL: apiServer.URL, PollIntervalSeconds: 5})
	job := claimedJob{
		ID:        "job-noprinter",
		Content:   json.RawMessage(`{"text":"test"}`),
		CreatedAt: time.Now().Format(time.RFC3339),
		Printer:   nil,
	}

	p.processJob(job, map[string]bool{})

	time.Sleep(100 * time.Millisecond)
	if statusUpdate != JobStatusFailed {
		t.Errorf("no-printer job should FAIL, got %q", statusUpdate)
	}
}

// ─── Poller Start/Stop ─────────────────────────────────────────────────────

func TestPoller_StartStop(t *testing.T) {
	callCount := 0
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		json.NewEncoder(w).Encode(claimResponse{Success: true, Data: []claimedJob{}})
	}))
	defer server.Close()

	p := NewPoller(Config{AdminAPIURL: server.URL, PollIntervalSeconds: 1, ServiceKey: "k"})
	p.Start()
	time.Sleep(2500 * time.Millisecond)
	p.Stop()

	mu.Lock()
	count := callCount
	mu.Unlock()

	// Should have polled at least 2 times (immediate + 1-2 ticks)
	if count < 2 {
		t.Errorf("expected at least 2 polls, got %d", count)
	}
}

func TestPoller_Stats(t *testing.T) {
	p := NewPoller(Config{PollIntervalSeconds: 5})
	processed, lastPoll := p.Stats()
	if processed != 0 {
		t.Errorf("initial processed = %d", processed)
	}
	if !lastPoll.IsZero() {
		t.Error("initial lastPoll should be zero")
	}

	p.jobsProcessed.Store(10)
	p.lastPollTime.Store(time.Now())

	processed, lastPoll = p.Stats()
	if processed != 10 {
		t.Errorf("processed = %d, want 10", processed)
	}
	if lastPoll.IsZero() {
		t.Error("lastPoll should be set")
	}
}

func TestPoller_JobAge(t *testing.T) {
	p := NewPoller(Config{PollIntervalSeconds: 5})

	recent := claimedJob{CreatedAt: time.Now().Add(-30 * time.Second).Format(time.RFC3339)}
	age := p.jobAge(recent)
	if age < 29*time.Second || age > 31*time.Second {
		t.Errorf("age = %v, expected ~30s", age)
	}

	old := claimedJob{CreatedAt: time.Now().Add(-5 * time.Minute).Format(time.RFC3339)}
	age = p.jobAge(old)
	if age < 4*time.Minute {
		t.Errorf("age = %v, expected ~5m", age)
	}

	invalid := claimedJob{CreatedAt: "not-a-date"}
	age = p.jobAge(invalid)
	if age != 0 {
		t.Errorf("invalid date age = %v, expected 0", age)
	}
}

// ─── Full E2E: Claim → Print → Status ──────────────────────────────────────

func TestPoller_E2E_ClaimPrintStatus(t *testing.T) {
	// Mock printer
	printerListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer printerListener.Close()
	printerPort := printerListener.Addr().(*net.TCPAddr).Port

	printReceived := make(chan []byte, 1)
	go func() {
		conn, err := printerListener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		data, _ := io.ReadAll(conn)
		printReceived <- data
	}()

	// Track API calls
	var claimCalled, statusCalled bool
	var finalStatus string

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/bridge/print-jobs/claim":
			claimCalled = true
			json.NewEncoder(w).Encode(claimResponse{
				Success: true,
				Data: []claimedJob{
					{
						ID:        "e2e-job-1",
						Content:   json.RawMessage(fmt.Sprintf(`{"header":"KOT #99","lines":[{"text":"1x Dal Makhani"}],"footer":"Kitchen"}`)),
						CreatedAt: time.Now().Format(time.RFC3339),
						Printer:   &jobPrinter{ID: "p1", IPAddress: "127.0.0.1", Port: printerPort},
					},
				},
			})
		default:
			statusCalled = true
			body, _ := io.ReadAll(r.Body)
			var payload map[string]string
			json.Unmarshal(body, &payload)
			finalStatus = payload["status"]
			w.WriteHeader(200)
		}
	}))
	defer apiServer.Close()

	// Run one poll cycle
	p := NewPoller(Config{AdminAPIURL: apiServer.URL, ServiceKey: "e2e-key", PollIntervalSeconds: 5})
	p.pollOnce()

	// Verify printer received ESC/POS data
	select {
	case data := <-printReceived:
		s := string(data)
		if !containsStr(s, "KOT #99") {
			t.Error("printer should receive KOT header")
		}
		if !containsStr(s, "Dal Makhani") {
			t.Error("printer should receive menu item")
		}
		if !containsBytes(data, []byte(escCut)) {
			t.Error("printer should receive cut command")
		}
		t.Logf("E2E: Printer received %d bytes of ESC/POS data", len(data))
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for print")
	}

	time.Sleep(200 * time.Millisecond)

	if !claimCalled {
		t.Error("claim endpoint not called")
	}
	if !statusCalled {
		t.Error("status endpoint not called")
	}
	if finalStatus != JobStatusCompleted {
		t.Errorf("final status = %q, want COMPLETED", finalStatus)
	}
	if p.jobsProcessed.Load() != 1 {
		t.Errorf("processed = %d, want 1", p.jobsProcessed.Load())
	}
}

func splitPath(path string) []string {
	parts := []string{}
	for _, p := range split(path, '/') {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

func split(s string, sep byte) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}
