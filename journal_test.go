package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func withTempHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func TestJournal_MarkAndClear_RoundTrip(t *testing.T) {
	withTempHome(t)

	journalMark("job-1", false)
	entries := loadJournal()
	if e, ok := entries["job-1"]; !ok || e.Printed {
		t.Fatalf("expected job-1 present with printed=false, got %+v (ok=%v)", e, ok)
	}

	journalMark("job-1", true)
	entries = loadJournal()
	if e, ok := entries["job-1"]; !ok || !e.Printed {
		t.Fatalf("expected job-1 present with printed=true, got %+v (ok=%v)", e, ok)
	}

	journalClear("job-1")
	entries = loadJournal()
	if _, ok := entries["job-1"]; ok {
		t.Fatal("expected job-1 to be cleared")
	}
}

func TestJournal_LoadJournal_MissingFile(t *testing.T) {
	withTempHome(t)

	entries := loadJournal()
	if len(entries) != 0 {
		t.Errorf("expected empty journal, got %d entries", len(entries))
	}
}

func TestJournal_LoadJournal_MalformedFile(t *testing.T) {
	withTempHome(t)

	os.MkdirAll(configDir(), 0755)
	os.WriteFile(journalPath(), []byte("{not json"), 0600)

	entries := loadJournal()
	if len(entries) != 0 {
		t.Errorf("expected empty journal on malformed file, got %d entries", len(entries))
	}
}

func TestJournal_ClearMissing_NoOp(t *testing.T) {
	withTempHome(t)

	// Clearing a job that was never marked should not create a journal file.
	journalClear("never-marked")
	if _, err := os.Stat(journalPath()); !os.IsNotExist(err) {
		t.Error("expected no journal file to be created")
	}
}

func TestReconcileJournal_PrintedTrue_ReReportsAndClears(t *testing.T) {
	withTempHome(t)

	var gotStatus string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		json.Unmarshal(body, &payload)
		gotStatus = payload["status"]
		w.WriteHeader(200)
	}))
	defer server.Close()

	journalMark("crashed-job", true)

	p := NewPoller(Config{AdminAPIURL: server.URL, ServiceKey: "key", PollIntervalSeconds: 5})
	p.reconcileJournal()

	if gotStatus != JobStatusCompleted {
		t.Errorf("expected re-reported status %q, got %q", JobStatusCompleted, gotStatus)
	}
	entries := loadJournal()
	if _, ok := entries["crashed-job"]; ok {
		t.Error("expected journal entry cleared after acknowledged re-report")
	}
}

func TestReconcileJournal_PrintedTrue_KeptOnFailedReport(t *testing.T) {
	withTempHome(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer server.Close()

	journalMark("crashed-job", true)

	p := NewPoller(Config{AdminAPIURL: server.URL, ServiceKey: "key", PollIntervalSeconds: 5})
	p.reconcileJournal()

	entries := loadJournal()
	if e, ok := entries["crashed-job"]; !ok || !e.Printed {
		t.Errorf("expected entry to remain for retry on next startup, got %+v (ok=%v)", e, ok)
	}
}

func TestReconcileJournal_PrintedTrue_ClearedOnJobNotFound(t *testing.T) {
	withTempHome(t)

	// resort-os returns 404 when the job no longer exists (e.g. pruned) — a
	// deleted job can't be duplicate-printed, so retrying forever is pointless.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer server.Close()

	journalMark("gone-job", true)

	p := NewPoller(Config{AdminAPIURL: server.URL, ServiceKey: "key", PollIntervalSeconds: 5})
	p.reconcileJournal()

	entries := loadJournal()
	if _, ok := entries["gone-job"]; ok {
		t.Error("expected entry cleared when resort-os reports the job as gone (404)")
	}
}

func TestReconcileJournal_PrintedFalse_DroppedWithoutHTTPCall(t *testing.T) {
	withTempHome(t)

	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	defer server.Close()

	journalMark("unconfirmed-job", false)

	p := NewPoller(Config{AdminAPIURL: server.URL, ServiceKey: "key", PollIntervalSeconds: 5})
	p.reconcileJournal()

	if called {
		t.Error("unconfirmed (printed=false) entries should not trigger a status re-report")
	}
	entries := loadJournal()
	if _, ok := entries["unconfirmed-job"]; ok {
		t.Error("expected unconfirmed entry to be dropped")
	}
}

func TestReconcileJournal_Empty_NoOp(t *testing.T) {
	withTempHome(t)

	p := NewPoller(Config{AdminAPIURL: "http://unused.invalid", PollIntervalSeconds: 5})
	p.reconcileJournal() // must not panic or write a file
	if _, err := os.Stat(journalPath()); !os.IsNotExist(err) {
		t.Error("expected no journal file for an empty reconcile")
	}
}
