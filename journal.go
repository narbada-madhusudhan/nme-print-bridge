package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// ─── Crash-Recovery Journal ─────────────────────────────────────────────────
//
// DESIGN PROPOSAL — flag for reviewer (journal format, reconcile semantics,
// idempotency assumptions all up for debate; this is a conservative first
// pass, not a final word):
//
// Between a successful print and the terminal status PATCH being
// acknowledged by resort-os, a bridge crash leaves the job stuck PRINTING
// server-side. resort-os's own recovery flips it back to PENDING after a
// timeout, so it gets re-claimed and re-printed — a duplicate physical
// receipt. This journal guards against that one failure mode:
//
//  1. Before attempting to print, record the job as claimed (printed=false).
//  2. Flip the entry to printed=true only once the print call itself
//     returns success.
//  3. Clear the entry once the terminal status PATCH is acknowledged
//     (2xx response).
//  4. On startup, any entry still marked printed=true means we sent data to
//     the printer but never confirmed resort-os saw the terminal status —
//     best-effort re-PATCH COMPLETED for it, then clear.
//  5. Entries still marked printed=false at startup were never confirmed
//     printed (the crash could have happened before printing even started,
//     or mid-write). We deliberately do NOT re-report these: we drop them
//     and defer to resort-os's existing PENDING-recovery + re-claim, i.e.
//     the pre-journal behavior. This assumes "unconfirmed" usually means
//     "not printed" — a partial TCP write that still reached the printer
//     is a residual duplicate-print risk this design does not close.
//     Closing it fully would need an idempotency key resort-os can dedupe
//     terminal-status writes on; out of scope for this bridge-side fix.
//
// Single-writer by construction (the poller processes jobs one at a time in
// its own goroutine), so a mutex + whole-file rewrite is sufficient — no
// real WAL/append-log needed for this volume.

var journalMu sync.Mutex

type journalEntry struct {
	JobID   string `json:"job_id"`
	Printed bool   `json:"printed"`
}

func journalPath() string {
	return filepath.Join(configDir(), JournalFile)
}

// loadJournal reads the journal file, keyed by job ID. Best-effort: a
// missing or malformed file yields an empty journal rather than an error.
func loadJournal() map[string]journalEntry {
	journalMu.Lock()
	defer journalMu.Unlock()
	return loadJournalLocked()
}

func loadJournalLocked() map[string]journalEntry {
	entries := map[string]journalEntry{}
	data, err := os.ReadFile(journalPath())
	if err != nil {
		return entries
	}
	var list []journalEntry
	if err := json.Unmarshal(data, &list); err != nil {
		log.Printf("[journal] Warning: failed to parse journal.json: %v", err)
		return entries
	}
	for _, e := range list {
		entries[e.JobID] = e
	}
	return entries
}

func saveJournalLocked(entries map[string]journalEntry) {
	list := make([]journalEntry, 0, len(entries))
	for _, e := range entries {
		list = append(list, e)
	}
	data, err := json.Marshal(list)
	if err != nil {
		return
	}
	// 0700 to match config.go's saveConfig hardening (#8). Note: on Windows
	// (resort front-desk PCs) Unix perm bits are a no-op — real lockdown
	// there needs an ACL; TODO if that's ever in scope.
	os.MkdirAll(configDir(), 0700)
	if err := os.WriteFile(journalPath(), data, 0600); err != nil {
		log.Printf("[journal] Warning: failed to write journal.json: %v", err)
	}
}

// journalMark records that jobID has been claimed and is about to be
// printed (printed=false), or has been successfully sent to the printer
// (printed=true).
func journalMark(jobID string, printed bool) {
	journalMu.Lock()
	defer journalMu.Unlock()
	entries := loadJournalLocked()
	entries[jobID] = journalEntry{JobID: jobID, Printed: printed}
	saveJournalLocked(entries)
}

// journalClear removes jobID from the journal — its terminal status was
// acknowledged, or it was never actually sent to a printer.
func journalClear(jobID string) {
	journalMu.Lock()
	defer journalMu.Unlock()
	entries := loadJournalLocked()
	if _, ok := entries[jobID]; !ok {
		return
	}
	delete(entries, jobID)
	saveJournalLocked(entries)
}

// reconcileJournal runs once at startup, before the poller starts claiming
// new jobs. See the package doc above for the reconcile semantics.
func (p *Poller) reconcileJournal() {
	entries := loadJournal()
	for jobID, e := range entries {
		if !e.Printed {
			log.Printf("[journal] Dropping unconfirmed entry for job %s (print was never confirmed)", jobID)
			journalClear(jobID)
			continue
		}
		log.Printf("[journal] Job %s printed but status unacked before last shutdown — re-reporting COMPLETED", jobID)
		if p.updateStatus(jobID, JobStatusCompleted, "") {
			journalClear(jobID)
		} else {
			log.Printf("[journal] Re-report for job %s failed, will retry next startup", jobID)
		}
	}
}
