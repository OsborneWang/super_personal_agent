package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Daemon is the long-lived agentd process
type Daemon struct {
	ledger *Ledger
	sm     *StateMachine
	server *Server
	config *Config

	mu      sync.Mutex
	running map[string]*exec.Cmd // ticketID -> worker process
	quit    chan struct{}
	wg      sync.WaitGroup

	homeDir  string
	pollTick time.Duration
}

// rulePaths returns the L2 rule files to inject into workers.
// Ticket-level rules override/extend config-level rules.
func (d *Daemon) rulePaths() []string {
	if d.config == nil {
		return nil
	}
	return d.config.Rules
}

// NewDaemon creates the daemon
func NewDaemon(homeDir string) (*Daemon, error) {
	ledgerPath := filepath.Join(homeDir, ".agent", "control", time.Now().Format("2006-01-02")+".jsonl")
	ledger, err := NewLedger(ledgerPath)
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}

	sm := NewStateMachine(ledger)
	config := LoadConfig(homeDir)

	d := &Daemon{
		ledger:   ledger,
		sm:       sm,
		config:   config,
		running:  make(map[string]*exec.Cmd),
		quit:     make(chan struct{}),
		homeDir:  homeDir,
		pollTick: 2 * time.Second,
	}

	return d, nil
}

// Start performs reconcile, opens socket, starts loops
func (d *Daemon) Start() error {
	// 1. Reconcile (startup recovery)
	reconciler := NewReconciler(d.sm, d.ledger)
	if err := reconciler.Reconcile(); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}

	// 2. Open socket
	sockPath := filepath.Join(d.homeDir, ".agent", "sock")
	server, err := NewServer(d, sockPath)
	if err != nil {
		return fmt.Errorf("start server: %w", err)
	}
	d.server = server

	// 3. Start loops
	d.wg.Add(3)
	go func() { defer d.wg.Done(); d.server.Serve() }()
	go func() { defer d.wg.Done(); d.scheduleLoop() }()
	go func() { defer d.wg.Done(); d.watchdogLoop() }()

	d.log("agentd started, socket=%s", sockPath)
	return nil
}

// Stop gracefully shuts down
func (d *Daemon) Stop() {
	close(d.quit)
	if d.server != nil {
		d.server.Close()
	}
	d.wg.Wait()
	d.ledger.Close()
}

// AdmitTicket validates and records a new ticket
func (d *Daemon) AdmitTicket(t *Ticket) error {
	if t.CreatedAt == "" {
		t.CreatedAt = time.Now().Format(time.RFC3339)
	}
	t.State = StatePending

	_, err := d.ledger.Append("ticket.admitted", t.ID, Actor{Actor: "user", Who: "user:<name>"}, map[string]interface{}{
		"spec": t,
	})
	if err != nil {
		return fmt.Errorf("record admit: %w", err)
	}

	// Load into state machine
	if err := d.sm.Replay(); err != nil {
		return fmt.Errorf("replay after admit: %w", err)
	}

	// Move to ready (resource check is minimal in M1)
	if err := d.sm.TransitionTicket(t.ID, StateReady, "resources_available", Actor{Actor: "agentd"}); err != nil {
		return err
	}

	return nil
}

// ClaimNext atomically claims the next ready ticket (used by CLI claim, mostly for testing)
func (d *Daemon) ClaimNext() (*Ticket, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, t := range d.sm.ListTickets() {
		if t.State == StateReady {
			if _, busy := d.running[t.ID]; busy {
				continue
			}
			if err := d.sm.TransitionTicket(t.ID, StateRunning, "claimed", Actor{Actor: "agentd"}); err != nil {
				continue
			}
			return t, nil
		}
	}
	return nil, nil
}

// AnswerTicket records the user's answer and resumes the worker with the
// same session-id so it continues with the new information.
func (d *Daemon) AnswerTicket(ticketID string, text string) error {
	t, err := d.sm.GetTicket(ticketID)
	if err != nil {
		return err
	}
	if t.State != StateWaitingUser {
		return fmt.Errorf("ticket %s is not waiting for user (state=%s)", ticketID, t.State)
	}

	// Record the answer
	if _, err := d.ledger.Append("answer.given", ticketID, Actor{Actor: "user", Who: "user:<name>"}, map[string]interface{}{
		"text": text,
	}); err != nil {
		return fmt.Errorf("record answer: %w", err)
	}

	// Transition back to running
	if err := d.sm.TransitionTicket(ticketID, StateRunning, "user_answered", Actor{Actor: "user", Who: "user:<name>"}); err != nil {
		return err
	}

	// Resume worker with the same session-id, feeding the answer
	t.SessionID = sessionIDOr(t)
	prompt := fmt.Sprintf("用户已经回答了你之前的问题。\n\n用户的回答：%s\n\n请基于这个回答继续完成任务。", text)

	d.mu.Lock()
	defer d.mu.Unlock()
	if _, busy := d.running[ticketID]; busy {
		return fmt.Errorf("worker already running for %s", ticketID)
	}

	cmd, err := d.spawnPiWorkerWithPrompt(t, prompt)
	if err != nil {
		d.sm.TransitionTicket(ticketID, StateFailed, "respawn_failed: "+err.Error(), Actor{Actor: "agentd"})
		return fmt.Errorf("respawn worker: %w", err)
	}
	d.running[ticketID] = cmd
	pid := cmd.Process.Pid
	d.sm.SetWorkerPID(ticketID, pid)
	d.ledger.Append("run.claimed", ticketID, Actor{Actor: "agentd", PID: pid}, map[string]interface{}{
		"run_id": fmt.Sprintf("r%d", t.Attempt+1),
		"pid":    pid,
		"resume": true,
	})
	d.log("resumed worker for %s (pid=%d) after answer", ticketID, pid)
	return nil
}

// sessionIDOr returns the ticket's session id, defaulting to the ticket id
func sessionIDOr(t *Ticket) string {
	if t.SessionID != "" {
		return t.SessionID
	}
	return t.ID
}

// scheduleLoop spawns workers for ready tickets
func (d *Daemon) scheduleLoop() {
	ticker := time.NewTicker(d.pollTick)
	defer ticker.Stop()

	for {
		select {
		case <-d.quit:
			return
		case <-ticker.C:
			d.spawnReadyWorkers()
		}
	}
}

// spawnReadyWorkers spawns a fake worker for each ready ticket without a running worker
func (d *Daemon) spawnReadyWorkers() {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, t := range d.sm.ListTickets() {
		if t.State != StateReady {
			continue
		}
		if _, busy := d.running[t.ID]; busy {
			continue
		}

		// Transition to running
		if err := d.sm.TransitionTicket(t.ID, StateRunning, "claimed_by_scheduler", Actor{Actor: "agentd"}); err != nil {
			continue
		}

		// Spawn real pi worker
		cmd, err := d.spawnPiWorker(t)
		if err != nil {
			d.log("spawn worker for %s failed: %v", t.ID, err)
			d.sm.TransitionTicket(t.ID, StateFailed, "spawn_failed: "+err.Error(), Actor{Actor: "agentd"})
			continue
		}

		d.running[t.ID] = cmd
		pid := cmd.Process.Pid

		// Update in-memory pid BEFORE recording so watchdog sees correct value
		d.sm.SetWorkerPID(t.ID, pid)

		// Record claim with pid
		d.ledger.Append("run.claimed", t.ID, Actor{Actor: "agentd", PID: pid}, map[string]interface{}{
			"run_id": fmt.Sprintf("r%d", t.Attempt+1),
			"pid":    pid,
		})

		d.log("spawned worker for %s (pid=%d)", t.ID, pid)
	}
}

// watchdogLoop checks for dead workers and waiting_user timeouts
func (d *Daemon) watchdogLoop() {
	ticker := time.NewTicker(d.pollTick)
	defer ticker.Stop()

	for {
		select {
		case <-d.quit:
			return
		case <-ticker.C:
			d.checkDeadWorkers()
		}
	}
}

// checkDeadWorkers transitions running tickets with dead pids back to ready
func (d *Daemon) checkDeadWorkers() {
	for _, t := range d.sm.ListTickets() {
		if t.State != StateRunning || t.WorkerPID == 0 {
			continue
		}
		if !isProcessAlive(t.WorkerPID) {
			d.log("worker for %s (pid=%d) is dead, transitioning to ready for retry", t.ID, t.WorkerPID)
			d.sm.SetWorkerPID(t.ID, 0) // clear so we don't re-detect the same dead pid
			d.sm.TransitionTicket(t.ID, StateReady, "worker_died_auto_retry", Actor{Actor: "agentd"})
		}
	}
}

func (d *Daemon) log(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "[%s] %s\n", time.Now().Format("15:04:05"), msg)
}
