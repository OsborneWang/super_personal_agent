package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	command := os.Args[1]

	var err error
	switch command {
	case "daemon":
		var rest []string
		if len(os.Args) > 3 {
			rest = os.Args[3:]
		}
		err = daemonSubcommand(arg(2), rest)
	case "init":
		err = cmdInit()
	case "test":
		err = cmdTest()
	case "reconcile":
		err = cmdReconcile()
	case "ping":
		err = cmdClient("ping", nil)
	case "ls":
		err = cmdList(listOpts{
			All:    hasFlag("--all", "-a"),
			Closed: hasFlag("--closed", "-c"),
		})
	case "show":
		err = cmdClient("show", map[string]interface{}{"id": arg(2)})
	case "admit":
		err = cmdAdmitDemo()
	case "answer":
		err = cmdClient("answer", map[string]interface{}{"id": arg(2), "text": strings.Join(os.Args[3:], " ")})
	case "follow":
		err = cmdFollow(arg(2))
	case "result":
		err = cmdResult(arg(2))
	case "tail":
		follow := false
		for _, a := range os.Args[3:] {
			if a == "-f" || a == "--follow" {
				follow = true
			}
		}
		err = cmdTail(arg(2), follow)
	case "close":
		err = cmdCloseMany(os.Args[2:])
	case "cancel":
		err = cmdCloseMany(os.Args[2:])
	default:
		log.Fatalf("Unknown command: %s", command)
	}

	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Println("super_personal_agent")
	fmt.Println()
	fmt.Println("Daemon (systemd user service):")
	fmt.Println("  agent daemon start          Start in background (auto-installs)")
	fmt.Println("  agent daemon stop           Stop")
	fmt.Println("  agent daemon status         Show status")
	fmt.Println("  agent daemon logs [-f]      Show/follow logs")
	fmt.Println("  agent daemon enable         Start on login")
	fmt.Println("  agent daemon                Run in foreground (debug)")
	fmt.Println("  agent daemon uninstall      Remove the service")
	fmt.Println()
	fmt.Println("CLI (talks to running agentd via socket):")
	fmt.Println("  agent ping                Health check")
	fmt.Println("  agent ls                  List all tickets")
	fmt.Println("  agent show <id>           Show ticket detail")
	fmt.Println("  agent admit               Admit a demo ticket")
	fmt.Println("  agent answer <id> <text>  Answer a waiting ticket")
	fmt.Println("  agent follow <id>         Stream ticket state changes live")
	fmt.Println("  agent result <id>         Show the worker's final result")
	fmt.Println("  agent tail <id> [-f]      Stream what the worker is doing (tool calls/output)")
	fmt.Println("  agent cancel <id>         Cancel a ticket")
	fmt.Println()
	fmt.Println("Dev/validation:")
	fmt.Println("  agent init                Initialize ledger")
	fmt.Println("  agent test                Run M0 test scenario")
	fmt.Println("  agent reconcile           Run reconcile once")
}

func arg(i int) string {
	if len(os.Args) > i {
		return os.Args[i]
	}
	return ""
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	return home
}

func sockPath() string {
	return filepath.Join(homeDir(), ".agent", "sock")
}

// ---- daemon ----

func cmdDaemon() error {
	d, err := NewDaemon(homeDir())
	if err != nil {
		return err
	}

	if err := d.Start(); err != nil {
		return err
	}

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	fmt.Fprintf(os.Stderr, "\nreceived %v, shutting down\n", sig)

	d.Stop()
	return nil
}

// ---- dev commands ----

func cmdInit() error {
	ledgerPath := filepath.Join(homeDir(), ".agent", "control", time.Now().Format("2006-01-02")+".jsonl")
	ledger, err := NewLedger(ledgerPath)
	if err != nil {
		return err
	}
	defer ledger.Close()

	fmt.Printf("Ledger initialized at: %s\n", ledgerPath)
	fmt.Printf("High water mark: %d\n", ledger.HighWater())
	return nil
}

func cmdTest() error {
	ledgerPath := filepath.Join(homeDir(), ".agent", "control", time.Now().Format("2006-01-02")+".jsonl")
	ledger, err := NewLedger(ledgerPath)
	if err != nil {
		return err
	}
	defer ledger.Close()

	sm := NewStateMachine(ledger)

	ticket := Ticket{
		ID:         "T-test-001",
		Goal:       "Test ticket for M0 validation",
		Acceptance: []string{"Test passes"},
		Context: map[string]interface{}{
			"user_intent": "Validate M0 implementation",
		},
		Depth:       1,
		MaxDepth:    3,
		SessionID:   "T-test-001",
		MaxAttempts: 3,
		CreatedAt:   time.Now().Format("2006-01-02T15:04:05Z07:00"),
	}

	_, err = ledger.Append("ticket.admitted", ticket.ID, Actor{Actor: "user"}, map[string]interface{}{
		"spec": ticket,
	})
	if err != nil {
		return fmt.Errorf("admit ticket: %w", err)
	}
	fmt.Println("✓ Ticket admitted")

	if err := sm.Replay(); err != nil {
		return fmt.Errorf("replay after admit: %w", err)
	}

	transitions := []struct {
		to     State
		reason string
	}{
		{StateReady, "resources_available"},
		{StateRunning, "claimed_by_worker"},
		{StateDone, "worker_completed"},
		{StateClosed, "cleanup_finished"},
	}
	for _, tr := range transitions {
		if err := sm.TransitionTicket(ticket.ID, tr.to, tr.reason, Actor{Actor: "agentd"}); err != nil {
			return fmt.Errorf("transition to %s: %w", tr.to, err)
		}
		fmt.Printf("✓ Transition to %s\n", tr.to)
	}

	sm2 := NewStateMachine(ledger)
	if err := sm2.Replay(); err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	ticket2, err := sm2.GetTicket(ticket.ID)
	if err != nil {
		return fmt.Errorf("get ticket after replay: %w", err)
	}
	if ticket2.State != StateClosed {
		return fmt.Errorf("expected closed, got %s", ticket2.State)
	}
	fmt.Println("✓ Replay successful, state matches")

	if err := sm2.TransitionTicket(ticket.ID, StateRunning, "invalid", Actor{Actor: "agentd"}); err == nil {
		return fmt.Errorf("expected invalid transition to fail")
	}
	fmt.Println("✓ Invalid transition rejected")

	fmt.Println("\n✓ All M0 tests passed")
	return nil
}

func cmdReconcile() error {
	ledgerPath := filepath.Join(homeDir(), ".agent", "control", time.Now().Format("2006-01-02")+".jsonl")
	ledger, err := NewLedger(ledgerPath)
	if err != nil {
		return err
	}
	defer ledger.Close()

	sm := NewStateMachine(ledger)
	reconciler := NewReconciler(sm, ledger)
	if err := reconciler.Reconcile(); err != nil {
		return err
	}

	fmt.Println("✓ Reconcile completed")
	fmt.Printf("  Tickets: %d\n", len(sm.ListTickets()))
	fmt.Printf("  High water: %d\n", ledger.HighWater())
	return nil
}

// ---- CLI client ----

func cmdClient(command string, args map[string]interface{}) error {
	client, err := NewClient(sockPath())
	if err != nil {
		return fmt.Errorf("connect to agentd (is it running? try 'agent daemon'): %w", err)
	}
	defer client.Close()

	resp, err := client.Call(command, args)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("agentd error: %s", resp.Error)
	}

	prettyPrint(resp.Data)
	return nil
}

// prettyPrint renders a value as indented JSON
func prettyPrint(v interface{}) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Println(v)
		return
	}
	fmt.Println(string(data))
}

// hasFlag reports whether any of the given flags appear in os.Args
func hasFlag(names ...string) bool {
	for _, a := range os.Args[1:] {
		for _, n := range names {
			if a == n {
				return true
			}
		}
	}
	return false
}

// listOpts controls which tickets the board shows
type listOpts struct {
	All    bool // include closed
	Closed bool // show only closed
}

// cmdCloseMany closes one or more tickets (验收完成/不再关注)
func cmdCloseMany(ids []string) error {
	// Drop flags
	var real []string
	for _, id := range ids {
		if strings.HasPrefix(id, "-") {
			continue
		}
		real = append(real, id)
	}
	if len(real) == 0 {
		return fmt.Errorf("usage: sa close <id> [<id> ...]")
	}

	client, err := NewClient(sockPath())
	if err != nil {
		return fmt.Errorf("connect to agentd: %w", err)
	}
	defer client.Close()

	var failed int
	for _, id := range real {
		resp, err := client.Call("close", map[string]interface{}{"id": id})
		if err != nil {
			fmt.Printf("  ✗ %-20s %v\n", id, err)
			failed++
			continue
		}
		if !resp.Success {
			fmt.Printf("  ✗ %-20s %s\n", id, resp.Error)
			failed++
			continue
		}
		fmt.Printf("  ✓ %-20s closed\n", id)
	}
	fmt.Printf("\n%d closed, %d failed\n", len(real)-failed, failed)
	return nil
}

// cmdList prints a management view of all tickets, grouped by project
func cmdList(opts listOpts) error {
	client, err := NewClient(sockPath())
	if err != nil {
		return fmt.Errorf("connect to agentd (is it running? try 'agent daemon'): %w", err)
	}
	defer client.Close()

	resp, err := client.Call("ls", nil)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("agentd: %s", resp.Error)
	}

	data, _ := json.Marshal(resp.Data)
	var tickets []Ticket
	json.Unmarshal(data, &tickets)

	if len(tickets) == 0 {
		fmt.Println("no tickets")
		return nil
	}

	// Filter by requested view
	filtered := tickets[:0]
	for _, t := range tickets {
		isClosed := t.State == StateClosed
		if opts.Closed {
			if isClosed {
				filtered = append(filtered, t)
			}
			continue
		}
		if opts.All || !isClosed {
			filtered = append(filtered, t)
		}
	}
	tickets = filtered

	if len(tickets) == 0 {
		if opts.Closed {
			fmt.Println("no closed tickets")
		} else {
			fmt.Println("no tickets (use 'sa ls --all' to include closed)")
		}
		return nil
	}

	// Group by project (empty project = global)
	groups := map[string][]Ticket{}
	var order []string
	for _, t := range tickets {
		proj := t.Project
		if proj == "" {
			proj = "(global)"
		}
		if _, seen := groups[proj]; !seen {
			order = append(order, proj)
		}
		groups[proj] = append(groups[proj], t)
	}

	for _, proj := range order {
		fmt.Printf("\n%s\n", proj)
		for _, t := range groups[proj] {
			marker := stateMarker(t.State)
			extra := ""
			if t.Depth > 1 {
				extra = fmt.Sprintf(" [L%d]", t.Depth)
			}
			fmt.Printf("  %s %-18s %-13s%s %s\n", marker, t.ID, t.State, extra, truncate(t.Goal, 55))
		}
	}
	return nil
}

func stateMarker(s State) string {
	switch s {
	case StateRunning:
		return "⚙️"
	case StateReady:
		return "⏳"
	case StatePending:
		return "·"
	case StateWaitingUser:
		return "⚠️"
	case StateDone:
		return "✅"
	case StateFailed:
		return "❌"
	case StateClosed:
		return "🔒"
	}
	return " "
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// cmdFollow waits on a ticket until it needs you or reaches a terminal state,
// then hands off with a clear verdict and the next command to run.
func cmdFollow(id string) error {
	if id == "" {
		return fmt.Errorf("usage: agent follow <id>")
	}
	client, err := NewClient(sockPath())
	if err != nil {
		return fmt.Errorf("connect to agentd: %w", err)
	}
	defer client.Close()

	getTicket := func() (*Ticket, error) {
		resp, err := client.Call("show", map[string]interface{}{"id": id})
		if err != nil {
			return nil, err
		}
		if !resp.Success {
			return nil, fmt.Errorf("agentd: %s", resp.Error)
		}
		data, _ := json.Marshal(resp.Data)
		var t Ticket
		json.Unmarshal(data, &t)
		return &t, nil
	}

	t, err := getTicket()
	if err != nil {
		return err
	}

	fmt.Printf("👀 蹲守 %s\n", id)
	fmt.Printf("   %s\n", truncate(t.Goal, 70))
	fmt.Printf("   \uff08Ctrl+C \u9000\u51fa\uff0c\u4e0d\u5f71\u54cd\u540e\u53f0\u6267\u884c\uff09\n\n")

	// Already in a state that needs the user
	if t.State == StateWaitingUser {
		handoffWaiting(t)
		return nil
	}
	if isTerminal(t.State) {
		handoffTerminal(t, 0)
		return nil
	}

	var runStart time.Time
	lastState := t.State
	for {
		time.Sleep(1 * time.Second)
		t, err := getTicket()
		if err != nil {
			return err
		}

		if t.State != lastState {
			switch t.State {
			case StateRunning:
				runStart = time.Now()
				fmt.Printf("⚙️  [%s] \u5f00\u59cb\u6267\u884c\n", time.Now().Format("15:04:05"))
			case StateWaitingUser:
				handoffWaiting(t)
				return nil
			case StateReady:
				// retry after worker death; keep waiting quietly
				fmt.Printf("🔁 [%s] \u91cd\u8bd5\u4e2d\uff08attempt %d\uff09\n", time.Now().Format("15:04:05"), t.Attempt)
			}
			lastState = t.State
		}

		if isTerminal(t.State) {
			elapsed := time.Duration(0)
			if !runStart.IsZero() {
				elapsed = time.Since(runStart)
			}
			handoffTerminal(t, elapsed)
			return nil
		}
	}
}

// handoffWaiting tells the user exactly what the worker needs and how to answer.
func handoffWaiting(t *Ticket) {
	fmt.Println("⚠️  \u9700\u8981\u4f60\u56de\u7b54")
	if t.Question != "" {
		fmt.Printf("\n   %s\n\n", indentLines(t.Question, "   "))
	} else {
		fmt.Println()
	}
	fmt.Printf("👉 \u56de\u7b54\uff1asa answer %s \"\u4f60\u7684\u56de\u7b54\"\n", t.ID)
}

// handoffTerminal reports the final verdict plus the next command.
func handoffTerminal(t *Ticket, elapsed time.Duration) {
	dur := ""
	if elapsed > 0 {
		dur = fmt.Sprintf("\uff08\u8017\u65f6 %s\uff09", elapsed.Round(time.Second))
	}
	switch t.State {
	case StateDone:
		fmt.Printf("✅ \u5b8c\u6210%s\n", dur)
		fmt.Printf("👉 \u67e5\u770b\u7ed3\u679c\uff1asa result %s\n", t.ID)
	case StateFailed:
		fmt.Printf("❌ \u5931\u8d25%s\n", dur)
		fmt.Printf("👉 \u770b\u8fc7\u7a0b\uff1asa tail %s\n", t.ID)
	case StateClosed:
		fmt.Printf("🔒 \u5df2\u5173\u95ed%s\n", dur)
	default:
		fmt.Printf("\u72b6\u6001\uff1a%s\n", t.State)
	}
}

func isTerminal(s State) bool {
	return s == StateDone || s == StateFailed || s == StateClosed
}

func indentLines(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}

// cmdResult prints a ticket's final result.
// Prefers the stable result.md artifact; falls back to scanning the trace.
func cmdResult(id string) error {
	if id == "" {
		return fmt.Errorf("usage: agent result <id>")
	}

	client, err := NewClient(sockPath())
	if err != nil {
		return fmt.Errorf("connect to agentd: %w", err)
	}
	resp, err := client.Call("show", map[string]interface{}{"id": id})
	client.Close()
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("agentd: %s", resp.Error)
	}
	data, _ := json.Marshal(resp.Data)
	var t Ticket
	json.Unmarshal(data, &t)

	root := t.Project
	if root == "" {
		root = homeDir()
	}
	dir := filepath.Join(root, ".agent", "tickets", id)

	// Header
	fmt.Printf("\n%s  [%s]\n", t.ID, t.State)
	fmt.Printf("%s\n\n", t.Goal)

	// 1. Preferred: stable artifact
	if b, err := os.ReadFile(filepath.Join(dir, "result.md")); err == nil && len(b) > 0 {
		fmt.Println(string(b))
		fmt.Println()
		return nil
	}

	// 2. Fallback: last assistant message from trace
	tracePath := filepath.Join(dir, "trace.jsonl")
	f, err := os.Open(tracePath)
	if err != nil {
		return fmt.Errorf("no result for %s (state=%s; worker may not have finished): %w", id, t.State, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	last := ""
	for scanner.Scan() {
		line := scanner.Bytes()
		var ev struct {
			Type    string `json:"type"`
			Message *struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.Type == "message_end" && ev.Message != nil && ev.Message.Role == "assistant" {
			if txt := extractText(ev.Message.Content); txt != "" {
				last = txt
			}
		}
	}
	if last == "" {
		return fmt.Errorf("no result for %s (state=%s)", id, t.State)
	}
	fmt.Println(last)
	return nil
}

// cmdTail streams what a worker is actually doing, by reading its trace.jsonl.
// Unlike `follow` (state transitions only), this shows live activity:
// tool calls, assistant messages, errors.
func cmdTail(id string, follow bool) error {
	if id == "" {
		return fmt.Errorf("usage: agent tail <id> [-f]")
	}

	// Ask agentd for the ticket so we know where its workspace lives
	client, err := NewClient(sockPath())
	if err != nil {
		return fmt.Errorf("connect to agentd: %w", err)
	}
	resp, err := client.Call("show", map[string]interface{}{"id": id})
	client.Close()
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("agentd: %s", resp.Error)
	}
	data, _ := json.Marshal(resp.Data)
	var t Ticket
	json.Unmarshal(data, &t)

	root := t.Project
	if root == "" {
		root = homeDir()
	}
	tracePath := filepath.Join(root, ".agent", "tickets", id, "trace.jsonl")

	// Wait briefly for the trace file to appear (ticket may just have started)
	var file *os.File
	for i := 0; i < 30; i++ {
		file, err = os.Open(tracePath)
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("trace not found for %s (has the worker started?): %w", id, err)
	}
	defer file.Close()

	fmt.Printf("tailing %s\n  trace: %s\n  (Ctrl+C to stop)\n\n", id, tracePath)

	// Read complete lines from the current offset. Returns the new offset.
	readFrom := func(offset int64) (int64, error) {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return offset, err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
		for scanner.Scan() {
			renderTraceLine(scanner.Bytes())
			// Track bytes consumed so a partial trailing line is retried later
			offset += int64(len(scanner.Bytes())) + 1
		}
		return offset, scanner.Err()
	}

	offset, err := readFrom(0)
	if err != nil {
		return err
	}
	if !follow {
		return nil
	}

	// Follow: poll for appended bytes
	for {
		time.Sleep(500 * time.Millisecond)
		info, statErr := file.Stat()
		if statErr != nil {
			return statErr
		}
		if info.Size() <= offset {
			continue
		}
		offset, err = readFrom(offset)
		if err != nil {
			return err
		}
	}
}

// renderTraceLine prints one pi JSONL event in a human-friendly way
func renderTraceLine(line []byte) {
	if len(line) == 0 {
		return
	}
	var ev struct {
		Type    string          `json:"type"`
		Message *struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		ToolName string          `json:"toolName"`
		Args     json.RawMessage `json:"args"`
		Result   json.RawMessage `json:"result"`
		IsError  bool            `json:"isError"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}
	ts := time.Now().Format("15:04:05")
	switch ev.Type {
	case "tool_execution_start":
		args := string(ev.Args)
		if len(args) > 160 {
			args = args[:160] + "…"
		}
		fmt.Printf("%s ⚙ %-12s %s\n", ts, ev.ToolName, strings.ReplaceAll(args, "\n", " "))
	case "tool_execution_end":
		mark := "✓"
		if ev.IsError {
			mark = "✗"
		}
		fmt.Printf("%s %s %s done\n", ts, mark, ev.ToolName)
	case "message_end":
		if ev.Message != nil && ev.Message.Role == "assistant" {
			text := extractText(ev.Message.Content)
			if text != "" {
				if len(text) > 400 {
					text = text[:400] + "…"
				}
				fmt.Printf("%s 💬 %s\n", ts, strings.ReplaceAll(text, "\n", " "))
			}
		}
	case "agent_settled":
		fmt.Printf("%s ━ worker settled\n", ts)
	}
}

func cmdAdmitDemo() error {
	ticket := Ticket{
		ID:         fmt.Sprintf("T-%d", time.Now().Unix()),
		Goal:       "Demo ticket: verify M1 daemon end-to-end flow works",
		Acceptance: []string{"Ticket transitions pending→ready→running→done automatically"},
		Context: map[string]interface{}{
			"user_intent": "Validate M1 daemon scheduling and fake worker",
		},
		Depth:       1,
		MaxDepth:    3,
		MaxAttempts: 3,
	}

	var specMap map[string]interface{}
	j, _ := jsonMarshal(ticket)
	jsonUnmarshal2(j, &specMap)

	return cmdClient("admit", map[string]interface{}{"spec": specMap})
}
