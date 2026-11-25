package main

import (
	"encoding/json"
	"fmt"
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
		err = cmdList()
	case "show":
		err = cmdClient("show", map[string]interface{}{"id": arg(2)})
	case "admit":
		err = cmdAdmitDemo()
	case "answer":
		err = cmdClient("answer", map[string]interface{}{"id": arg(2), "text": strings.Join(os.Args[3:], " ")})
	case "follow":
		err = cmdFollow(arg(2))
	case "cancel":
		err = cmdClient("cancel", map[string]interface{}{"id": arg(2)})
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
	fmt.Println("  agent follow <id>         Stream ticket events live")
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

// cmdList prints a management view of all tickets, grouped by project
func cmdList() error {
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

// cmdFollow polls the ticket and prints state changes until terminal
func cmdFollow(id string) error {
	if id == "" {
		return fmt.Errorf("usage: agent follow <id>")
	}
	client, err := NewClient(sockPath())
	if err != nil {
		return fmt.Errorf("connect to agentd: %w", err)
	}
	defer client.Close()

	fmt.Printf("Following %s (Ctrl+C to stop)\n", id)
	var lastState string
	var lastAttempt int
	for {
		resp, err := client.Call("show", map[string]interface{}{"id": id})
		if err != nil {
			return err
		}
		if !resp.Success {
			return fmt.Errorf("agentd: %s", resp.Error)
		}
		data, _ := json.Marshal(resp.Data)
		var t Ticket
		json.Unmarshal(data, &t)

		if string(t.State) != lastState || t.Attempt != lastAttempt {
			marker := "  "
			switch t.State {
			case StateRunning:
				marker = "⚙️ "
			case StateWaitingUser:
				marker = "⚠️ "
			case StateDone:
				marker = "✅ "
			case StateFailed:
				marker = "❌ "
			case StateClosed:
				marker = "🔒 "
			}
			fmt.Printf("%s[%s] %-13s (attempt %d)\n", marker, time.Now().Format("15:04:05"), t.State, t.Attempt)
			lastState = string(t.State)
			lastAttempt = t.Attempt
		}

		if t.State == StateDone || t.State == StateFailed || t.State == StateClosed {
			fmt.Printf("terminal state reached: %s\n", t.State)
			return nil
		}
		time.Sleep(1 * time.Second)
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
