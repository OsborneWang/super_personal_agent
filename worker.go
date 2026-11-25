package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// piEvent is a single line of pi --mode json output
type piEvent struct {
	Type    string          `json:"type"`
	Message *piMessage      `json:"message,omitempty"`
	Aborted bool            `json:"aborted,omitempty"`
	Usage   *piUsage        `json:"usage,omitempty"`
	Raw     json.RawMessage `json:"-"`
}

type piMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Model   string          `json:"model,omitempty"`
	Usage   *piUsage        `json:"usage,omitempty"`
}

type piUsage struct {
	Input  int      `json:"input"`
	Output int      `json:"output"`
	Cost   *piCost  `json:"cost,omitempty"`
}

type piCost struct {
	Total float64 `json:"total"`
}

// workerResult is what the daemon learns from a finished worker
type workerResult struct {
	outcome    string // done | failed | need_input
	finalText  string
	costUSD    float64
	model      string
	lastEvent  string
	err        error
}

// buildWorkerPrompt builds the task prompt injected into the worker
func buildWorkerPrompt(t *Ticket) string {
	var b strings.Builder
	b.WriteString("# 任务\n\n")
	fmt.Fprintf(&b, "%s\n\n", t.Goal)
	b.WriteString("## 验收标准（必须逐条自检，全部满足才算完成）\n\n")
	for i, a := range t.Acceptance {
		fmt.Fprintf(&b, "%d. %s\n", i+1, a)
	}
	b.WriteString("\n## 上下文\n\n")
	if t.Context != nil {
		for k, v := range t.Context {
			fmt.Fprintf(&b, "- **%s**: %v\n", k, v)
		}
	}
	b.WriteString("\n## 规则\n\n")
	b.WriteString("- 遇到无法遵守的约束、需要用户决策的阻塞问题时，立即停止并在最后输出一行 `NEED_INPUT: <你的问题>`。\n")
	b.WriteString("- 如果你用 create_subtask 拆分了子任务，且需要子任务结果才能继续：完成你自己能做的部分后，输出一行 `WAITING_FOR_CHILDREN` 然后结束。系统会在子任务全部完成后，把结果交还给你继续。\n")
	b.WriteString("- 完成后输出一行 `DONE: <一句话总结>`，并在正文中说明每条验收标准的达成情况。\n")
	b.WriteString("- 遇到无法恢复的失败，输出一行 `FAILED: <原因>`。\n")
	b.WriteString("- 不要编造验收结果；做不到就如实报告。\n")
	b.WriteString("- 重要：create_subtask 创建的是异步子任务，你在当前这轮拿不到它的结果。需要结果就输出 WAITING_FOR_CHILDREN，不要凭空填写子任务的答案。\n")
	return b.String()
}

// buildWorkerSystemPrompt builds the appended system prompt (L2 rules + worker identity)
func buildWorkerSystemPrompt(t *Ticket, rules []string) string {
	var b strings.Builder
	b.WriteString("你是 super_personal_agent 的一名 Worker，负责独立完成分配给你的任务。\n\n")
	if len(rules) > 0 {
		b.WriteString("## 项目权威约束（违反任何一条都视为任务失败，必须立即停止并报告）\n\n")
		for _, r := range rules {
			fmt.Fprintf(&b, "%s\n\n", r)
		}
	}
	if tc, ok := t.Context["_task_constraints"].([]interface{}); ok && len(tc) > 0 {
		b.WriteString("## 本任务约束\n\n")
		for _, c := range tc {
			fmt.Fprintf(&b, "- %v\n", c)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// loadRules reads L2 rule files declared by the adapter (best-effort)
func loadRules(paths []string) []string {
	var rules []string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		rules = append(rules, fmt.Sprintf("### %s\n\n%s", filepath.Base(p), string(data)))
	}
	return rules
}

// spawnPiWorker starts a real pi worker and streams its events to the trace file
func (d *Daemon) spawnPiWorker(t *Ticket) (*exec.Cmd, error) {
	return d.spawnPiWorkerWithPrompt(t, buildWorkerPrompt(t))
}

// spawnPiWorkerWithPrompt starts a pi worker with an explicit prompt.
// Used both for the initial run and for resuming after an answer.
// The worker's physical workspace lives under the ticket's PROJECT directory,
// so it can read/write the project's files directly.
func (d *Daemon) spawnPiWorkerWithPrompt(t *Ticket, prompt string) (*exec.Cmd, error) {
	// Workspace + trace live under <project>/.agent/tickets/<id>/
	root := d.projectRoot(t)
	workDir := filepath.Join(root, ".agent", "tickets", t.ID, "work")
	traceDir := filepath.Join(root, ".agent", "tickets", t.ID)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir work: %w", err)
	}
	traceFile, err := os.OpenFile(
		filepath.Join(traceDir, "trace.jsonl"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open trace: %w", err)
	}

	// System prompt: L2 rules (global + project) + task identity
	rules := loadRules(d.rulePaths(t))
	sysPrompt := buildWorkerSystemPrompt(t, rules)
	sysFile := filepath.Join(traceDir, ".sysprompt.txt")
	if err := os.WriteFile(sysFile, []byte(sysPrompt), 0644); err != nil {
		traceFile.Close()
		return nil, fmt.Errorf("write sysprompt: %w", err)
	}

	// Model resolution: ticket override > project config > global config
	model := resolveWorkerModel(t, d.effectiveConfig(t))

	// Use --session-id so the worker's conversation persists across
	// need_input -> answer resume. The session id is bound to the ticket.
	args := []string{
		"--mode", "json", "-p",
		"--session-id", t.SessionID,
		"--append-system-prompt", sysFile,
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, prompt)

	cmd := exec.Command("pi", args...)
	cmd.Dir = workDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Inject worker identity so the create_subtask tool knows its parent + depth
	cmd.Env = append(os.Environ(),
		"SUPER_TICKET_ID="+t.ID,
		fmt.Sprintf("SUPER_TICKET_DEPTH=%d", t.Depth),
		"SUPER_SOCK="+filepath.Join(d.homeDir, ".agent", "sock"),
		"SUPER_PROJECT="+t.Project,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		traceFile.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		traceFile.Close()
		return nil, fmt.Errorf("start pi: %w", err)
	}

	// Stream events to trace + parse outcome in background
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer traceFile.Close()
		result := d.consumeWorkerEvents(t, stdout, traceFile)

		cmd.Wait()

		d.mu.Lock()
		delete(d.running, t.ID)
		d.mu.Unlock()

		d.finishWorker(t, result)
	}()

	return cmd, nil
}

// consumeWorkerEvents reads pi JSONL, writes trace, extracts outcome
func (d *Daemon) consumeWorkerEvents(t *Ticket, stdout io.Reader, trace *os.File) workerResult {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	res := workerResult{outcome: "done"} // default; refined below
	var finalText strings.Builder

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Persist raw event to trace
		trace.Write(line)
		trace.Write([]byte{'\n'})

		var ev piEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		res.lastEvent = ev.Type

		switch ev.Type {
		case "message_end":
			if ev.Message != nil {
				if ev.Message.Usage != nil {
					if ev.Message.Usage.Cost != nil {
						res.costUSD = ev.Message.Usage.Cost.Total
					}
				}
				if ev.Message.Model != "" {
					res.model = ev.Message.Model
				}
				if ev.Message.Role == "assistant" {
					text := extractText(ev.Message.Content)
					if text != "" {
						finalText.Reset()
						finalText.WriteString(text)
					}
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		res.err = err
		res.outcome = "failed"
		return res
	}

	res.finalText = finalText.String()
	res.outcome = classifyOutcome(res.finalText)
	return res
}

// classifyOutcome reads the worker's final text for control markers
func classifyOutcome(text string) string {
	upper := strings.ToUpper(text)
	switch {
	case strings.Contains(upper, "NEED_INPUT:"):
		return "need_input"
	case strings.Contains(upper, "FAILED:"):
		return "failed"
	case strings.Contains(upper, "WAITING_FOR_CHILDREN"):
		return "wait_children"
	case strings.Contains(upper, "DONE:"):
		return "done"
	default:
		// No explicit marker: treat completion without error as done
		return "done"
	}
}

// extractText pulls text content out of a pi message content array
func extractText(content json.RawMessage) string {
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// writeResult saves the worker's final output as a stable artifact next to
// the trace, so `sa result <id>` can show it without parsing the event stream.
func (d *Daemon) writeResult(t *Ticket, res workerResult) {
	if res.finalText == "" {
		return
	}
	dir := filepath.Join(d.projectRoot(t), ".agent", "tickets", t.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}
	path := filepath.Join(dir, "result.md")
	_ = os.WriteFile(path, []byte(res.finalText), 0644)
}

// finishWorker applies the worker outcome to the state machine
func (d *Daemon) finishWorker(t *Ticket, res workerResult) {
	// Always release resources when the worker exits, regardless of outcome.
	defer d.rm.Release(t.ID)

	// Persist the final output as a stable artifact (best-effort)
	d.writeResult(t, res)

	ticket, err := d.sm.GetTicket(t.ID)
	if err != nil {
		return
	}
	if ticket.State != StateRunning {
		return // cancelled or already moved
	}

	// Record run.ended with usage
	d.ledger.Append("run.ended", t.ID, Actor{Actor: "worker", PID: ticket.WorkerPID}, map[string]interface{}{
		"outcome":   res.outcome,
		"cost_usd":  res.costUSD,
		"model":     res.model,
		"final_len": len(res.finalText),
	})

	switch res.outcome {
	case "wait_children":
		// Parent explicitly delegated and is waiting for children results.
		// Mark it so the watchdog does NOT treat the exited worker as dead.
		d.sm.SetWaitingChildren(t.ID, true)
		d.sm.SetWorkerPID(t.ID, 0)
		d.log("worker %s waiting for children to complete", t.ID)
		d.ledger.Append("note", t.ID, Actor{Actor: "agentd"}, map[string]interface{}{
			"text": "parent waiting for children",
		})
		return

	case "done":
		// If this worker spawned subtasks that are still running but it did NOT
		// explicitly wait, it finished its own part. Wait for children before
		// marking done so results can be folded in on resume.
		if d.sm.HasUnfinishedChildren(t.ID) {
			d.sm.SetWaitingChildren(t.ID, true)
			d.sm.SetWorkerPID(t.ID, 0)
			d.log("worker %s finished its part but has unfinished children; waiting", t.ID)
			d.ledger.Append("note", t.ID, Actor{Actor: "agentd"}, map[string]interface{}{
				"text": "parent finished own work, waiting for children",
			})
			return
		}
		d.sm.TransitionTicket(t.ID, StateDone, "worker_completed", Actor{Actor: "worker", PID: ticket.WorkerPID})
		d.notify(t, "quiet", "done", fmt.Sprintf("✅ %s 完成", t.ID))
		d.log("worker done for %s (cost=$%.4f model=%s)", t.ID, res.costUSD, res.model)

	case "need_input":
		question := extractMarker(res.finalText, "NEED_INPUT:")
		d.ledger.Append("question.asked", t.ID, Actor{Actor: "worker", PID: ticket.WorkerPID}, map[string]interface{}{
			"question_id": fmt.Sprintf("q-%d", time.Now().Unix()),
			"text":        question,
		})
		d.sm.TransitionTicket(t.ID, StateWaitingUser, "worker_needs_input", Actor{Actor: "worker", PID: ticket.WorkerPID})
		d.notify(t, "interrupt", "need_input", fmt.Sprintf("⚠️ %s 需要你: %s", t.ID, question))
		d.log("worker needs input for %s: %s", t.ID, question)

	case "failed":
		reason := extractMarker(res.finalText, "FAILED:")
		if reason == "" {
			reason = "worker reported failure"
		}
		d.sm.TransitionTicket(t.ID, StateFailed, "worker_failed: "+reason, Actor{Actor: "worker", PID: ticket.WorkerPID})
		d.notify(t, "interrupt", "failed", fmt.Sprintf("❌ %s 失败: %s", t.ID, reason))
		d.log("worker failed for %s: %s", t.ID, reason)
	}
}

// extractMarker pulls the text after a control marker line
func extractMarker(text, marker string) string {
	idx := strings.Index(strings.ToUpper(text), strings.ToUpper(marker))
	if idx < 0 {
		return ""
	}
	rest := text[idx+len(marker):]
	// Take up to end of line
	if nl := strings.Index(rest, "\n"); nl >= 0 {
		rest = rest[:nl]
	}
	return strings.TrimSpace(rest)
}

// resolveWorkerModel: ticket override > config default
func resolveWorkerModel(t *Ticket, cfg *Config) string {
	if t.Model != nil && t.Model.Worker != "" {
		return t.Model.Worker
	}
	if cfg != nil && cfg.Model.Worker != "" {
		return cfg.Model.Worker
	}
	return "" // let pi decide
}
