package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Daemon is the long-lived agentd process
type Daemon struct {
	ledger *Ledger
	sm     *StateMachine
	server *Server
	config *Config
	rm     *ResourceManager

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
		rm:       NewResourceManager(),
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

// validateSubtask enforces recursion rules for a child ticket.
// Hard limit: a subtask at depth 3 cannot be created if parent is at depth 3
// (i.e. depth must be <= max_depth=3). When a worker at the max depth tries
// to split further, the parent is escalated to waiting_user instead.
func (d *Daemon) validateSubtask(child *Ticket) error {
	parent, err := d.sm.GetTicket(child.ParentID)
	if err != nil {
		return fmt.Errorf("parent ticket %s not found", child.ParentID)
	}
	// Depth must be parent.depth + 1 (worker sets this; we enforce consistency)
	expectedDepth := parent.Depth + 1
	if child.Depth != expectedDepth {
		child.Depth = expectedDepth // normalize
	}
	maxDepth := parent.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 3
	}
	child.MaxDepth = maxDepth
	if child.Depth > maxDepth {
		return fmt.Errorf("max recursion depth %d exceeded (parent depth=%d)", maxDepth, parent.Depth)
	}
	// Subagent model inheritance: child inherits parent's worker model unless overridden
	if child.Model == nil && parent.Model != nil {
		child.Model = parent.Model
	}
	// Session-id: each subtask gets its own, bound to its own ticket id
	if child.SessionID == "" {
		child.SessionID = child.ID
	}
	return nil
}

// CancelTicket transitions a ticket to closed and reclaims its resources.
func (d *Daemon) CancelTicket(id string) error {
	t, err := d.sm.GetTicket(id)
	if err != nil {
		return err
	}
	if t.State == StateClosed {
		return fmt.Errorf("ticket %s already closed", id)
	}
	if err := d.sm.TransitionTicket(id, StateClosed, "user_cancel", Actor{Actor: "user", Who: "user:<name>"}); err != nil {
		return err
	}
	d.closeTicket(t)
	d.notify(t, "quiet", "closed", fmt.Sprintf("🔒 %s 已取消", id))
	return nil
}

// closeTicket performs two-phase resource reclamation for a terminal ticket:
// 1. Kill the worker process group (if any is still alive)
// 2. Release all resource leases
// It is idempotent.
func (d *Daemon) closeTicket(t *Ticket) {
	// Phase 1: kill any lingering worker process group
	d.mu.Lock()
	cmd, running := d.running[t.ID]
	delete(d.running, t.ID)
	d.mu.Unlock()

	if running && cmd != nil && cmd.Process != nil {
		pid := cmd.Process.Pid
		// Kill the whole process group (negative pid)
		if err := syscall.Kill(-pid, syscall.SIGTERM); err == nil {
			d.log("sent SIGTERM to process group %d for ticket %s", pid, t.ID)
			// Give it a moment, then SIGKILL
			time.Sleep(2 * time.Second)
			if isProcessAlive(pid) {
				syscall.Kill(-pid, syscall.SIGKILL)
				d.log("sent SIGKILL to process group %d for ticket %s", pid, t.ID)
			}
		}
	}

	// Phase 2: release all resource leases held by this ticket
	d.rm.Release(t.ID)
	d.ledger.Append("resource.released", t.ID, Actor{Actor: "agentd"}, map[string]interface{}{
		"keys": resourceKeys(t.Resources),
	})
	d.log("closed ticket %s: resources released", t.ID)
}

// resourceKeys extracts the keys from resource requirements
func resourceKeys(reqs []ResourceReq) []string {
	keys := make([]string, 0, len(reqs))
	for _, r := range reqs {
		keys = append(keys, r.Key)
	}
	return keys
}

// notifyMaxDepth escalates a worker that hit the recursion ceiling.
// The parent ticket is moved to waiting_user so a human can decide whether
// the AI went off track or the requirement was misunderstood.
func (d *Daemon) notifyMaxDepth(t *Ticket) {
	d.ledger.Append("question.asked", t.ID, Actor{Actor: "agentd"}, map[string]interface{}{
		"question_id": fmt.Sprintf("q-maxdepth-%d", time.Now().Unix()),
		"text":        "任务已到达最大递归深度(3层)仍未完成。可能是 AI 走偏或需求理解有误，需要人工介入判断。",
		"kind":        "max_depth",
	})
	d.sm.TransitionTicket(t.ID, StateWaitingUser, "max_depth_reached", Actor{Actor: "agentd"})
	d.notify(t, "interrupt", "max_depth", fmt.Sprintf("⚠️ %s 达到最大递归深度仍未完成，需要人工介入", t.ID))
	d.log("ticket %s hit max recursion depth, escalated to user", t.ID)
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

		// Resource mutual exclusion: acquire before claiming.
		// On conflict, leave the ticket ready and record why it is not running.
		if err := d.rm.Acquire(t.ID, t.Resources); err != nil {
			d.log("ticket %s waiting for resource: %v", t.ID, err)
			continue
		}

		// Transition to running
		if err := d.sm.TransitionTicket(t.ID, StateRunning, "claimed_by_scheduler", Actor{Actor: "agentd"}); err != nil {
			d.rm.Release(t.ID) // rollback resource on transition failure
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
			d.checkParentCompletion()
			d.checkWaitingTimeouts()
		}
	}
}

// checkDeadWorkers transitions running tickets with dead pids back to ready
func (d *Daemon) checkDeadWorkers() {
	for _, t := range d.sm.ListTickets() {
		// Skip tickets waiting for their children (worker exited by design)
		if t.WaitingChildren {
			continue
		}
		if t.State != StateRunning || t.WorkerPID == 0 {
			continue
		}
		if !isProcessAlive(t.WorkerPID) {
			// If waiting on children, the "death" is expected (worker exited)
			if d.sm.HasUnfinishedChildren(t.ID) {
				continue
			}
			// Retry budget: too many deaths -> fail instead of infinite retry
			maxAttempts := 3
			if d.config != nil && d.config.MaxAttempts > 0 {
				maxAttempts = d.config.MaxAttempts
			}
			if t.Attempt >= maxAttempts {
				d.sm.SetWorkerPID(t.ID, 0)
				d.sm.TransitionTicket(t.ID, StateFailed, fmt.Sprintf("max_attempts_exceeded(%d)", maxAttempts), Actor{Actor: "agentd"})
				d.notify(t, "interrupt", "failed", fmt.Sprintf("❌ %s 超过最大重试次数(%d)，已标记失败", t.ID, maxAttempts))
				d.log("ticket %s failed: max attempts %d exceeded", t.ID, maxAttempts)
				continue
			}
			d.log("worker for %s (pid=%d) is dead, transitioning to ready for retry", t.ID, t.WorkerPID)
			d.sm.SetWorkerPID(t.ID, 0) // clear so we don't re-detect the same dead pid
			d.sm.TransitionTicket(t.ID, StateReady, "worker_died_auto_retry", Actor{Actor: "agentd"})
		}
	}
}

// checkWaitingTimeouts fails tickets that have been waiting_user too long.
func (d *Daemon) checkWaitingTimeouts() {
	timeoutMin := 30
	if d.config != nil && d.config.WaitingTimeoutMinutes > 0 {
		timeoutMin = d.config.WaitingTimeoutMinutes
	}
	now := time.Now()
	for _, t := range d.sm.ListTickets() {
		if t.State != StateWaitingUser || t.WaitingSince == "" {
			continue
		}
		since, err := time.Parse("2006-01-02T15:04:05Z07:00", t.WaitingSince)
		if err != nil {
			continue
		}
		if now.Sub(since) > time.Duration(timeoutMin)*time.Minute {
			d.sm.TransitionTicket(t.ID, StateFailed, "waiting_user_timeout", Actor{Actor: "agentd"})
			d.notify(t, "interrupt", "failed", fmt.Sprintf("❌ %s 等待回答超时(%d分钟)，已标记失败", t.ID, timeoutMin))
			d.log("ticket %s failed: waiting_user timeout", t.ID)
		}
	}
}

// checkParentCompletion resumes a waiting parent once all its children are
// done. The parent worker is restarted with its SAME session-id and the
// children's results, so it can fold them into its final output. If any child
// failed, the parent is failed directly.
func (d *Daemon) checkParentCompletion() {
	for _, t := range d.sm.ListTickets() {
		if t.State != StateRunning {
			continue
		}
		children := d.sm.ChildrenOf(t.ID)
		if len(children) == 0 {
			continue
		}
		// Only consider parents whose own worker already exited
		if _, stillRunning := d.running[t.ID]; stillRunning {
			continue
		}
		allDone := true
		anyFailed := false
		for _, c := range children {
			if c.State == StateFailed {
				anyFailed = true
			}
			if c.State != StateDone && c.State != StateClosed && c.State != StateFailed {
				allDone = false
				break
			}
		}
		if !allDone {
			continue
		}

		if anyFailed {
			d.sm.TransitionTicket(t.ID, StateFailed, "child_failed", Actor{Actor: "agentd"})
			d.notify(t, "interrupt", "failed", fmt.Sprintf("❌ %s 的子任务失败", t.ID))
			d.log("parent %s failed: child failed", t.ID)
			continue
		}

		// All children done: resume the parent with their results so it can
		// integrate them into its final deliverable (symmetric with answer-resume).
		d.resumeParentWithChildren(t, children)
	}
}

// resumeParentWithChildren restarts a waiting parent worker, feeding it the
// results of its finished children.
func (d *Daemon) resumeParentWithChildren(t *Ticket, children []*Ticket) {
	// Clear the waiting flag before resuming so watchdog treats it as live
	d.sm.SetWaitingChildren(t.ID, false)

	var b strings.Builder
	b.WriteString("你之前拆分的子任务已经全部完成。以下是它们的结果：\n\n")
	for _, c := range children {
		fmt.Fprintf(&b, "## 子任务 %s\n目标：%s\n", c.ID, c.Goal)
		result := d.readChildResult(c)
		fmt.Fprintf(&b, "结果：\n%s\n\n", result)
	}
	b.WriteString("请基于以上子任务结果，整合并完成你最初任务的剩余部分。完成后输出 DONE: 标记。")

	t.SessionID = sessionIDOr(t)

	d.mu.Lock()
	defer d.mu.Unlock()
	if _, busy := d.running[t.ID]; busy {
		return
	}
	cmd, err := d.spawnPiWorkerWithPrompt(t, b.String())
	if err != nil {
		d.log("resume parent %s failed: %v", t.ID, err)
		d.sm.TransitionTicket(t.ID, StateFailed, "resume_failed: "+err.Error(), Actor{Actor: "agentd"})
		return
	}
	d.running[t.ID] = cmd
	pid := cmd.Process.Pid
	d.sm.SetWorkerPID(t.ID, pid)
	d.ledger.Append("run.claimed", t.ID, Actor{Actor: "agentd", PID: pid}, map[string]interface{}{
		"run_id": fmt.Sprintf("r%d", t.Attempt+1),
		"pid":    pid,
		"resume": "children_done",
	})
	d.log("resumed parent %s with children results (pid=%d)", t.ID, pid)
}

// readChildResult extracts a child worker's final assistant text from its trace
func (d *Daemon) readChildResult(c *Ticket) string {
	tracePath := filepath.Join(d.homeDir, ".agent", "tickets", c.ID, "trace.jsonl")
	data, err := os.ReadFile(tracePath)
	if err != nil {
		return "(结果不可用)"
	}
	lines := strings.Split(string(data), "\n")
	// Walk backwards to find the last assistant message_end
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var ev piEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Type == "message_end" && ev.Message != nil && ev.Message.Role == "assistant" {
			text := extractText(ev.Message.Content)
			if text != "" {
				if len(text) > 2000 {
					text = text[:2000] + "\n...(截断)"
				}
				return text
			}
		}
	}
	return "(无最终输出)"
}

func (d *Daemon) log(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "[%s] %s\n", time.Now().Format("15:04:05"), msg)
}
