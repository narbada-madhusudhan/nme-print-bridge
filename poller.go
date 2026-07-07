package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

// ─── Print Job Poller ──────────────────────────────────────────────────────

type Poller struct {
	config               Config
	client               *http.Client
	stopCh               chan struct{}
	doneCh               chan struct{}
	jobsProcessed        atomic.Int64
	lastPollTime         atomic.Value // stores time.Time
	statusUpdateFailures atomic.Int64
}

// statusUpdateBackoffs is the delay before each retry of a failed status PATCH.
// Overridden in tests to avoid real sleeps.
var statusUpdateBackoffs = []time.Duration{1 * time.Second, 2 * time.Second, 5 * time.Second}

type claimedJob struct {
	ID        string          `json:"id"`
	Content   json.RawMessage `json:"content"`
	CreatedAt string          `json:"created_at"`
	Printer   *jobPrinter     `json:"printer"`
}

type jobPrinter struct {
	ID          string  `json:"id"`
	IPAddress   string  `json:"ip_address"`
	Port        int     `json:"port"`
	PrinterName *string `json:"printer_name"`
}

type claimResponse struct {
	Success bool         `json:"success"`
	Data    []claimedJob `json:"data"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func NewPoller(cfg Config) *Poller {
	return &Poller{
		config: cfg,
		client: &http.Client{Timeout: time.Duration(PollClaimTimeout) * time.Second},
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

// startPoller creates a Poller for cfg, publishes it as the active poller,
// replays the crash-recovery journal, then starts polling. Both process
// startup (main.go) and a live poll-config update (handlers.go
// handleSetPollConfig) go through this — reconcileJournal must always run
// before Start(), and routing both call sites through one helper is what
// keeps that ordering from drifting on one path but not the other (M4).
func startPoller(cfg Config) *Poller {
	poller := NewPoller(cfg)
	activePollerPtr.Store(poller)
	poller.reconcileJournal()
	poller.Start()
	return poller
}

func (p *Poller) Start() {
	go func() {
		defer close(p.doneCh)
		interval := time.Duration(p.config.PollIntervalSeconds) * time.Second
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// Run immediately on start
		p.pollOnce()

		for {
			select {
			case <-ticker.C:
				p.pollOnce()
			case <-p.stopCh:
				return
			}
		}
	}()
}

func (p *Poller) Stop() {
	close(p.stopCh)
	<-p.doneCh
}

func (p *Poller) Stats() (int64, time.Time) {
	processed := p.jobsProcessed.Load()
	lastPoll, _ := p.lastPollTime.Load().(time.Time)
	return processed, lastPoll
}

// StatusUpdateFailures returns the count of status PATCHes that exhausted
// all retries without succeeding.
func (p *Poller) StatusUpdateFailures() int64 {
	return p.statusUpdateFailures.Load()
}

// ─── Core Polling Logic ────────────────────────────────────────────────────

func (p *Poller) pollOnce() {
	p.lastPollTime.Store(time.Now())

	jobs, err := p.claimJobs()
	if err != nil {
		log.Printf("[poller] Claim failed: %v", err)
		return
	}
	if len(jobs) == 0 {
		return
	}

	// Get reachable printers for multi-hub routing
	reachable := p.getReachablePrinters()

	for _, job := range jobs {
		p.processJob(job, reachable)
	}
}

func (p *Poller) claimJobs() ([]claimedJob, error) {
	url := fmt.Sprintf("%s/api/bridge/print-jobs/claim", p.config.AdminAPIURL)

	req, err := http.NewRequest("POST", url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.config.ServiceKey != "" {
		req.Header.Set("X-Bridge-Key", p.config.ServiceKey)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		return nil, fmt.Errorf("unauthorized — check service_key in config")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API returned %d", resp.StatusCode)
	}

	var result claimResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("invalid response: %w", err)
	}

	if !result.Success {
		msg := "unknown error"
		if result.Error != nil {
			msg = result.Error.Message
		}
		return nil, fmt.Errorf("claim failed: %s", msg)
	}

	return result.Data, nil
}

func (p *Poller) getReachablePrinters() map[string]bool {
	printers, err := listPrintersCached()
	if err != nil {
		return map[string]bool{}
	}
	reachable := make(map[string]bool, len(printers))
	for _, pr := range printers {
		if pr.Enabled {
			reachable[pr.Name] = true
		}
	}
	return reachable
}

func (p *Poller) processJob(job claimedJob, reachable map[string]bool) {
	printerName := ""
	if job.Printer != nil && job.Printer.PrinterName != nil {
		printerName = *job.Printer.PrinterName
	}

	// Multi-hub reachability check
	if printerName != "" && len(reachable) > 0 && !reachable[printerName] {
		jobAge := p.jobAge(job)
		if jobAge >= time.Duration(UnreachableTimeoutSec)*time.Second {
			p.updateStatus(job.ID, JobStatusUnreachable, "Printer unreachable — no hub connected")
			log.Printf("[poller] Job %s: printer %q unreachable (age %s), marked UNREACHABLE", job.ID, printerName, jobAge)
		} else {
			// Release back to PENDING for another hub — no retry (M3): a
			// lost release here just waits out UnreachableTimeoutSec on the
			// next poll instead of risking a stale PENDING landing after
			// another hub has already re-claimed the job, which would cause
			// a duplicate print.
			p.reportStatusNoRetry(job.ID, JobStatusPending, "")
		}
		return
	}

	// Convert content to ESC/POS
	escposData := contentToEscPos(job.Content)

	// Crash-recovery journal: record the claim before printing so a crash
	// between print and status-ack doesn't cause a duplicate reprint. See
	// journal.go for the full design + reconcile semantics.
	journalMark(job.ID, false)

	// Print
	var printErr error
	if printerName != "" {
		printErr = printToUSB(printerName, escposData)
	} else if job.Printer != nil && job.Printer.IPAddress != "" {
		printErr = tcpSend(job.Printer.IPAddress, job.Printer.Port, escposData)
	} else {
		printErr = fmt.Errorf("no printer configured for job")
	}

	if printErr != nil {
		journalClear(job.ID)
		p.updateStatus(job.ID, JobStatusFailed, printErr.Error())
		log.Printf("[poller] Job %s: print failed: %v", job.ID, printErr)
		return
	}

	// Print succeeded — flip the marker. A crash from here on is recovered
	// at next startup by reconcileJournal re-reporting COMPLETED.
	journalMark(job.ID, true)

	if p.updateStatus(job.ID, JobStatusCompleted, "") {
		journalClear(job.ID)
	}
	p.jobsProcessed.Add(1)
	target := printerName
	if target == "" && job.Printer != nil {
		target = job.Printer.IPAddress
	}
	log.Printf("[poller] Job %s: printed to %s", job.ID, target)
}

// updateStatus PATCHes the job's status to resort-os, retrying with backoff
// on transient failure (network error or non-2xx/404 response). It returns
// whether the report is "settled" from the bridge's point of view:
//   - 2xx → settled (accepted, or a safe no-op if the job was already
//     terminal)
//   - 404 → settled (the job no longer exists server-side — e.g. already
//     reconciled by another hub — so nothing is left to duplicate-print;
//     this is NOT counted as a failure)
//   - retries exhausted → not settled, and statusUpdateFailures is bumped
//
// Callers use the return value to decide whether it's safe to clear the
// crash-recovery journal entry for the job (see journal.go). A transient
// failure that's never retried would leave the job stuck PRINTING until
// resort-os's stale-recovery resets it to PENDING, risking a duplicate
// print — so we make a bounded effort to land the final status.
//
// This is the retrying path for terminal reports (COMPLETED/FAILED/
// UNREACHABLE) and the startup journal replay. The PENDING-release path in
// processJob deliberately does NOT go through this — see reportStatusNoRetry.
func (p *Poller) updateStatus(jobID, status, errMsg string) bool {
	url := fmt.Sprintf("%s/api/bridge/print-jobs/%s/status", p.config.AdminAPIURL, jobID)

	payload := map[string]string{"status": status}
	if errMsg != "" {
		payload["error_message"] = errMsg
	}
	body, _ := json.Marshal(payload)

	maxAttempts := len(statusUpdateBackoffs) + 1
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			// Thread stopCh through the wait so Stop()/SIGTERM isn't blocked
			// for the full backoff schedule — a closed stopCh aborts the
			// retry immediately (job is left unsettled; the journal/PENDING
			// recovery paths pick it back up).
			select {
			case <-time.After(statusUpdateBackoffs[attempt-2]):
			case <-p.stopCh:
				log.Printf("[poller] Status update for %s aborted (shutting down)", jobID)
				p.statusUpdateFailures.Add(1)
				return false
			}
		}
		settled, retry, err := p.sendStatusUpdate(url, body)
		if err != nil {
			lastErr = err
			log.Printf("[poller] Status update for %s failed (attempt %d/%d): %v", jobID, attempt, maxAttempts, err)
		}
		if !retry {
			return settled
		}
	}

	p.statusUpdateFailures.Add(1)
	log.Printf("[poller] Status update for %s FAILED after %d attempts, giving up: %v", jobID, maxAttempts, lastErr)
	return false
}

// reportStatusNoRetry sends a single, non-retrying status PATCH. Used only
// for the PENDING-release path in processJob (printer unreachable but not
// yet timed out): retrying there risks a stale PENDING landing after
// another hub has already re-claimed the job, which would cause a duplicate
// print. A lost release here just waits out UnreachableTimeoutSec on the
// next poll instead. Failures here are logged but not counted against
// statusUpdateFailures — they're expected to self-heal via the next poll.
func (p *Poller) reportStatusNoRetry(jobID, status, errMsg string) {
	url := fmt.Sprintf("%s/api/bridge/print-jobs/%s/status", p.config.AdminAPIURL, jobID)
	payload := map[string]string{"status": status}
	if errMsg != "" {
		payload["error_message"] = errMsg
	}
	body, _ := json.Marshal(payload)

	if _, _, err := p.sendStatusUpdate(url, body); err != nil {
		log.Printf("[poller] Status update (no-retry) for %s failed: %v", jobID, err)
	}
}

// sendStatusUpdate performs a single PATCH attempt. It returns:
//   - settled: true when this outcome is final and needs no retry (2xx or
//     404)
//   - retry: true when the caller should retry (network/request error, or
//     any other non-2xx/404 response)
//   - err: the observed error, if any, for logging
func (p *Poller) sendStatusUpdate(url string, body []byte) (settled bool, retry bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(PollStatusUpdateTimeout)*time.Second)
	defer cancel()

	req, reqErr := http.NewRequestWithContext(ctx, "PATCH", url, bytes.NewReader(body))
	if reqErr != nil {
		return false, true, reqErr
	}
	req.Header.Set("Content-Type", "application/json")
	if p.config.ServiceKey != "" {
		req.Header.Set("X-Bridge-Key", p.config.ServiceKey)
	}

	resp, doErr := p.client.Do(req)
	if doErr != nil {
		return false, true, doErr
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return true, false, nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, false, nil
	}
	return false, true, fmt.Errorf("status update returned %d", resp.StatusCode)
}

func (p *Poller) jobAge(job claimedJob) time.Duration {
	t, err := time.Parse(time.RFC3339, job.CreatedAt)
	if err != nil {
		return 0
	}
	return time.Since(t)
}
