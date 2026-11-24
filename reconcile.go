package main

import (
	"fmt"
	"os"
	"strings"
)

// Reconciler handles startup recovery
type Reconciler struct {
	sm     *StateMachine
	ledger *Ledger
}

// NewReconciler creates a new reconciler
func NewReconciler(sm *StateMachine, ledger *Ledger) *Reconciler {
	return &Reconciler{
		sm:     sm,
		ledger: ledger,
	}
}

// Reconcile performs startup recovery
func (r *Reconciler) Reconcile() error {
	// 1. Replay ledger to rebuild state
	if err := r.sm.Replay(); err != nil {
		return fmt.Errorf("replay state: %w", err)
	}

	// 2. Check for orphaned running tickets
	orphaned := r.findOrphanedTickets()

	// 3. Handle orphaned tickets
	for _, ticket := range orphaned {
		if err := r.handleOrphanedTicket(ticket); err != nil {
			return fmt.Errorf("handle orphaned ticket %s: %w", ticket.ID, err)
		}
	}

	// 4. Record reconcile event
	_, err := r.ledger.Append("reconcile.applied", "", Actor{Actor: "agentd"}, map[string]interface{}{
		"orphaned_tickets": len(orphaned),
		"actions":          []string{"replayed_state", "handled_orphans"},
	})
	if err != nil {
		return fmt.Errorf("record reconcile: %w", err)
	}

	return nil
}

// findOrphanedTickets finds tickets that are running but their worker is dead
func (r *Reconciler) findOrphanedTickets() []*Ticket {
	var orphaned []*Ticket

	tickets := r.sm.ListTickets()
	for _, ticket := range tickets {
		if ticket.State == StateRunning && ticket.WorkerPID > 0 {
			// Check if process is alive
			if !isProcessAlive(ticket.WorkerPID) {
				orphaned = append(orphaned, ticket)
			}
		}
	}

	return orphaned
}

// handleOrphanedTicket handles a ticket whose worker died
func (r *Reconciler) handleOrphanedTicket(ticket *Ticket) error {
	// Worker died, transition back to ready for retry
	// The session-id is preserved, so new worker will inherit context
	return r.sm.TransitionTicket(
		ticket.ID,
		StateReady,
		"worker_died_auto_retry",
		Actor{Actor: "agentd"},
	)
}

// isProcessAlive checks if a process with given PID is alive.
// Uses /proc directly: more reliable than Signal(0) after a daemon restart,
// where the daemon is no longer the worker's parent.
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// /proc/<pid> existing means the process is alive
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
		// Also verify it's not a zombie
		if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			// stat field 3 is the state; 'Z' = zombie
			fields := strings.Fields(string(data))
			if len(fields) >= 3 && fields[2] == "Z" {
				return false
			}
		}
		return true
	}
	return false
}
