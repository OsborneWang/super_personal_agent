package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// State represents the ticket state
type State string

const (
	StatePending     State = "pending"
	StateReady       State = "ready"
	StateRunning     State = "running"
	StateWaitingUser State = "waiting_user"
	StateDone        State = "done"
	StateFailed      State = "failed"
	StateClosed      State = "closed"
)

// Transition represents a state transition
type Transition struct {
	From State
	To   State
}

// Allowed transitions table
var transitions = map[Transition]bool{
	{StatePending, StateReady}:       true,
	{StateReady, StateRunning}:       true,
	{StateRunning, StateWaitingUser}: true,
	{StateWaitingUser, StateRunning}: true,
	{StateRunning, StateDone}:        true,
	{StateRunning, StateFailed}:      true,
	{StateRunning, StateReady}:       true, // Worker died, retry
	{StateDone, StateClosed}:         true,
	{StateFailed, StateClosed}:       true,
	{StateWaitingUser, StateClosed}:  true,
	{StatePending, StateClosed}:      true, // Cancel
	{StateReady, StateClosed}:        true, // Cancel
	{StateRunning, StateClosed}:      true, // Cancel
}

// CanTransition checks if a state transition is allowed
func CanTransition(from, to State) bool {
	return transitions[Transition{From: from, To: to}]
}

// Ticket represents a task ticket
type Ticket struct {
	ID          string                 `json:"id"`
	State       State                  `json:"state"`
	Goal        string                 `json:"goal"`
	Acceptance  []string               `json:"acceptance"`
	Context     map[string]interface{} `json:"context"`
	Depth       int                    `json:"depth"`
	MaxDepth    int                    `json:"max_depth"`
	ParentID    string                 `json:"parent_id,omitempty"`
	SessionID   string                 `json:"session_id"`
	WorkerPID   int                    `json:"worker_pid,omitempty"`
	Attempt     int                    `json:"attempt"`
	MaxAttempts int                    `json:"max_attempts"`
	Model       *ModelSpec             `json:"model,omitempty"`
	Rules       []string               `json:"rules,omitempty"`
	Resources   []ResourceReq          `json:"resources,omitempty"`
	Project     string                 `json:"project,omitempty"` // Router cwd at admit time; immutable
	WaitingSince string                `json:"waiting_since,omitempty"`
	WaitingChildren bool               `json:"waiting_children,omitempty"`
	CreatedAt   string                 `json:"created_at"`
	UpdatedAt   string                 `json:"updated_at"`
}

// ResourceReq declares a resource the ticket needs to run.
// Kind and Key are opaque to the kernel; Mode is exclusive|shared.
type ResourceReq struct {
	Kind string `json:"kind"`           // worktree | port | gpu | ...
	Key  string `json:"key"`            // e.g. <project>:worktree:<big-change>
	Mode string `json:"mode,omitempty"` // exclusive (default) | shared
}

// ModelSpec holds model selection for a ticket
type ModelSpec struct {
	Router   string `json:"router,omitempty"`
	Worker   string `json:"worker,omitempty"`
	Subagent string `json:"subagent,omitempty"`
}

// StateMachine manages ticket states
type StateMachine struct {
	mu      sync.RWMutex
	tickets map[string]*Ticket
	ledger  *Ledger
}

// NewStateMachine creates a new state machine
func NewStateMachine(ledger *Ledger) *StateMachine {
	return &StateMachine{
		tickets: make(map[string]*Ticket),
		ledger:  ledger,
	}
}

// Replay rebuilds state from ledger events
func (sm *StateMachine) Replay() error {
	events, err := sm.ledger.Replay()
	if err != nil {
		return fmt.Errorf("replay ledger: %w", err)
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	for _, event := range events {
		if err := sm.applyEvent(event); err != nil {
			return fmt.Errorf("apply event %d: %w", event.Seq, err)
		}
	}

	return nil
}

// applyEvent applies a single event to state
func (sm *StateMachine) applyEvent(event *Event) error {
	switch event.Kind {
	case "ticket.admitted":
		return sm.applyTicketAdmitted(event)
	case "state.changed":
		return sm.applyStateChanged(event)
	case "run.claimed":
		return sm.applyRunClaimed(event)
	default:
		// Unknown event type - skip for forward compatibility
		return nil
	}
}

func (sm *StateMachine) applyTicketAdmitted(event *Event) error {
	var data struct {
		Spec Ticket `json:"spec"`
	}
	if err := jsonUnmarshal(event.Data, &data); err != nil {
		return err
	}

	ticket := &data.Spec
	ticket.State = StatePending
	sm.tickets[ticket.ID] = ticket
	return nil
}

func (sm *StateMachine) applyStateChanged(event *Event) error {
	var data struct {
		From   State  `json:"from"`
		To     State  `json:"to"`
		Reason string `json:"reason"`
	}
	if err := jsonUnmarshal(event.Data, &data); err != nil {
		return err
	}

	ticket, exists := sm.tickets[event.Ticket]
	if !exists {
		return fmt.Errorf("ticket %s not found", event.Ticket)
	}

	if !CanTransition(data.From, data.To) {
		return fmt.Errorf("invalid transition %s -> %s", data.From, data.To)
	}

	if data.To == StateRunning && data.Reason == "resumed_after_children" {
		ticket.WaitingChildren = false
	}
	ticket.State = data.To
	if data.To == StateWaitingUser {
		ticket.WaitingSince = event.At.Format("2006-01-02T15:04:05Z07:00")
	} else {
		ticket.WaitingSince = ""
	}
	return nil
}

func (sm *StateMachine) applyRunClaimed(event *Event) error {
	var data struct {
		RunID string `json:"run_id"`
		PID   int    `json:"pid"`
	}
	if err := jsonUnmarshal(event.Data, &data); err != nil {
		return err
	}

	ticket, exists := sm.tickets[event.Ticket]
	if !exists {
		return fmt.Errorf("ticket %s not found", event.Ticket)
	}

	ticket.WorkerPID = data.PID
	ticket.Attempt++
	ticket.UpdatedAt = event.At.Format("2006-01-02T15:04:05Z07:00")
	return nil
}

// SetWorkerPID updates the worker pid on a ticket (in-memory, after spawn)
func (sm *StateMachine) SetWorkerPID(ticketID string, pid int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if t, ok := sm.tickets[ticketID]; ok {
		t.WorkerPID = pid
	}
}

// SetWaitingChildren marks a ticket as waiting for its children to finish
func (sm *StateMachine) SetWaitingChildren(ticketID string, waiting bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if t, ok := sm.tickets[ticketID]; ok {
		t.WaitingChildren = waiting
	}
}

// TransitionTicket attempts to transition a ticket to a new state
func (sm *StateMachine) TransitionTicket(ticketID string, to State, reason string, by Actor) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	ticket, exists := sm.tickets[ticketID]
	if !exists {
		return fmt.Errorf("ticket %s not found", ticketID)
	}

	from := ticket.State
	if !CanTransition(from, to) {
		return fmt.Errorf("invalid transition %s -> %s for ticket %s", from, to, ticketID)
	}

	// Record state change event
	_, err := sm.ledger.Append("state.changed", ticketID, by, map[string]interface{}{
		"from":   from,
		"to":     to,
		"reason": reason,
	})
	if err != nil {
		return fmt.Errorf("record state change: %w", err)
	}

	if to == StateWaitingUser {
		ticket.WaitingSince = time.Now().Format("2006-01-02T15:04:05Z07:00")
	} else {
		ticket.WaitingSince = ""
	}
	ticket.State = to
	return nil
}

// GetTicket retrieves a ticket by ID
func (sm *StateMachine) GetTicket(id string) (*Ticket, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	ticket, exists := sm.tickets[id]
	if !exists {
		return nil, fmt.Errorf("ticket %s not found", id)
	}

	return ticket, nil
}

// ListTickets returns all tickets
func (sm *StateMachine) ListTickets() []*Ticket {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	tickets := make([]*Ticket, 0, len(sm.tickets))
	for _, ticket := range sm.tickets {
		tickets = append(tickets, ticket)
	}

	return tickets
}

// ChildrenOf returns all direct child tickets of a parent
func (sm *StateMachine) ChildrenOf(parentID string) []*Ticket {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	var children []*Ticket
	for _, t := range sm.tickets {
		if t.ParentID == parentID {
			children = append(children, t)
		}
	}
	return children
}

// HasUnfinishedChildren reports whether a ticket has children not yet done/closed
func (sm *StateMachine) HasUnfinishedChildren(parentID string) bool {
	for _, c := range sm.ChildrenOf(parentID) {
		if c.State != StateDone && c.State != StateClosed && c.State != StateFailed {
			return true
		}
	}
	return false
}

// Helper function for JSON unmarshaling
func jsonUnmarshal(data []byte, v interface{}) error {
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, v)
}
