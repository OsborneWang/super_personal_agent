package main

import (
	"fmt"
	"os/exec"
)

// notify sends a desktop notification with dedup via the ledger
func (d *Daemon) notify(t *Ticket, level string, kind string, message string) {
	// Record notification event (enables "how many times was I paged" analysis)
	d.ledger.Append("notification.sent", t.ID, Actor{Actor: "agentd"}, map[string]interface{}{
		"level":   level,
		"kind":    kind,
		"message": message,
	})

	// Fire desktop notification (best-effort, non-blocking)
	go func() {
		var urgency string
		switch level {
		case "interrupt":
			urgency = "critical"
		default:
			urgency = "normal"
		}
		// notify-send is optional; ignore errors (headless environments)
		_ = exec.Command("notify-send", "-u", urgency, "super_agent", message).Run()
	}()

	d.log("notify[%s] %s", level, message)
}

// helper for future use
func notifyPath(homeDir string) string {
	return fmt.Sprintf("%s/.agent", homeDir)
}
