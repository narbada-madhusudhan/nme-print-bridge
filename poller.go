package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// ─── Print Job Poller ──────────────────────────────────────────────────────

type Poller struct {
	config        Config
	client        *http.Client
	stopCh        chan struct{}
	doneCh        chan struct{}
	jobsProcessed atomic.Int64
	lastPollTime  atomic.Value // stores time.Time
}

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

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result claimResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
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
			p.updateStatus(job.ID, "UNREACHABLE", "Printer unreachable — no hub connected")
			log.Printf("[poller] Job %s: printer %q unreachable (age %s), marked UNREACHABLE", job.ID, printerName, jobAge)
		} else {
			// Release back to PENDING for another hub
			p.updateStatus(job.ID, "PENDING", "")
		}
		return
	}

	// Convert content to ESC/POS
	escposData := contentToEscPos(job.Content)

	// Print
	var printErr error
	if printerName != "" {
		printErr = printToUSB(printerName, escposData)
	} else if job.Printer != nil && job.Printer.IPAddress != "" {
		printErr = p.printNetwork(job.Printer.IPAddress, job.Printer.Port, escposData)
	} else {
		printErr = fmt.Errorf("no printer configured for job")
	}

	if printErr != nil {
		p.updateStatus(job.ID, "FAILED", printErr.Error())
		log.Printf("[poller] Job %s: print failed: %v", job.ID, printErr)
		return
	}

	p.updateStatus(job.ID, "COMPLETED", "")
	p.jobsProcessed.Add(1)
	target := printerName
	if target == "" && job.Printer != nil {
		target = job.Printer.IPAddress
	}
	log.Printf("[poller] Job %s: printed to %s", job.ID, target)
}

func (p *Poller) printNetwork(ip string, port int, data []byte) error {
	if port == 0 {
		port = DefaultPrinterPort
	}
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, time.Duration(NetworkDialTimeout)*time.Second)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(time.Duration(NetworkWriteTimeout) * time.Second))
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", addr, err)
	}
	return nil
}

func (p *Poller) updateStatus(jobID, status, errMsg string) {
	url := fmt.Sprintf("%s/api/bridge/print-jobs/%s/status", p.config.AdminAPIURL, jobID)

	payload := map[string]string{"status": status}
	if errMsg != "" {
		payload["error_message"] = errMsg
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("PATCH", url, bytes.NewReader(body))
	if err != nil {
		log.Printf("[poller] Status update request failed: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if p.config.ServiceKey != "" {
		req.Header.Set("X-Bridge-Key", p.config.ServiceKey)
	}

	client := &http.Client{Timeout: time.Duration(PollStatusUpdateTimeout) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[poller] Status update for %s failed: %v", jobID, err)
		return
	}
	resp.Body.Close()
}

func (p *Poller) jobAge(job claimedJob) time.Duration {
	t, err := time.Parse(time.RFC3339, job.CreatedAt)
	if err != nil {
		return 0
	}
	return time.Since(t)
}
