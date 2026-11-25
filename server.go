package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

// Request is a command from CLI to agentd
type Request struct {
	ID      string                 `json:"id,omitempty"`
	Command string                 `json:"command"`
	Args    map[string]interface{} `json:"args,omitempty"`
}

// Response is agentd's reply to CLI
type Response struct {
	ID      string      `json:"id,omitempty"`
	Success bool        `json:"success"`
	Error   string      `json:"error,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

// Server is the unix socket server
type Server struct {
	daemon   *Daemon
	listener net.Listener
	wg       sync.WaitGroup
	quit     chan struct{}
}

// NewServer creates a socket server bound to the daemon
func NewServer(daemon *Daemon, sockPath string) (*Server, error) {
	// Remove stale socket
	os.Remove(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", sockPath, err)
	}
	// Restrict permissions
	os.Chmod(sockPath, 0600)

	return &Server{
		daemon:   daemon,
		listener: listener,
		quit:     make(chan struct{}),
	}, nil
}

// Serve accepts connections until Close
func (s *Server) Serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return
			default:
				continue
			}
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// Close stops the server
func (s *Server) Close() {
	close(s.quit)
	s.listener.Close()
	s.wg.Wait()
}

func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	encoder := json.NewEncoder(conn)

	for scanner.Scan() {
		var req Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			encoder.Encode(Response{Success: false, Error: "invalid json: " + err.Error()})
			continue
		}

		resp := s.dispatch(&req)
		resp.ID = req.ID
		if err := encoder.Encode(resp); err != nil {
			return
		}
	}
}

func (s *Server) dispatch(req *Request) Response {
	switch req.Command {
	case "ping":
		return Response{Success: true, Data: map[string]interface{}{"pong": true, "time": time.Now()}}

	case "admit":
		return s.cmdAdmit(req)

	case "ls":
		return s.cmdList(req)

	case "show":
		return s.cmdShow(req)

	case "transition":
		return s.cmdTransition(req)

	case "claim":
		return s.cmdClaim(req)

	case "answer":
		return s.cmdAnswer(req)

	case "close":
		return s.cmdClose(req)

	case "cancel":
		return s.cmdCancel(req)

	default:
		return Response{Success: false, Error: "unknown command: " + req.Command}
	}
}

func (s *Server) cmdAdmit(req *Request) Response {
	specRaw, ok := req.Args["spec"]
	if !ok {
		return Response{Success: false, Error: "missing spec"}
	}

	specJSON, err := json.Marshal(specRaw)
	if err != nil {
		return Response{Success: false, Error: "invalid spec: " + err.Error()}
	}

	var ticket Ticket
	if err := json.Unmarshal(specJSON, &ticket); err != nil {
		return Response{Success: false, Error: "parse ticket: " + err.Error()}
	}

	// Hard validations (file_ticket equivalent)
	if err := validateTicket(&ticket); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	// Recursion: if this is a subtask, enforce parent + depth rules
	if ticket.ParentID != "" {
		if err := s.daemon.validateSubtask(&ticket); err != nil {
			return Response{Success: false, Error: err.Error()}
		}
	}

	if err := s.daemon.AdmitTicket(&ticket); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	return Response{Success: true, Data: map[string]interface{}{"ticket_id": ticket.ID}}
}

func (s *Server) cmdList(req *Request) Response {
	tickets := s.daemon.sm.ListTickets()
	return Response{Success: true, Data: tickets}
}

func (s *Server) cmdShow(req *Request) Response {
	id, _ := req.Args["id"].(string)
	if id == "" {
		return Response{Success: false, Error: "missing id"}
	}
	ticket, err := s.daemon.sm.GetTicket(id)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	return Response{Success: true, Data: ticket}
}

func (s *Server) cmdTransition(req *Request) Response {
	id, _ := req.Args["id"].(string)
	toStr, _ := req.Args["to"].(string)
	reason, _ := req.Args["reason"].(string)
	if id == "" || toStr == "" {
		return Response{Success: false, Error: "missing id or to"}
	}
	err := s.daemon.sm.TransitionTicket(id, State(toStr), reason, Actor{Actor: "user", Who: "user:<name>"})
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	return Response{Success: true}
}

func (s *Server) cmdClaim(req *Request) Response {
	ticket, err := s.daemon.ClaimNext()
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if ticket == nil {
		return Response{Success: true, Data: nil} // nothing to claim
	}
	return Response{Success: true, Data: ticket}
}

func (s *Server) cmdAnswer(req *Request) Response {
	id, _ := req.Args["id"].(string)
	text, _ := req.Args["text"].(string)
	if id == "" || text == "" {
		return Response{Success: false, Error: "missing id or text"}
	}
	if err := s.daemon.AnswerTicket(id, text); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	return Response{Success: true}
}

// cmdClose is the user-facing "验收完成/不再关注" action: move to closed and
// reclaim resources. Idempotent.
func (s *Server) cmdClose(req *Request) Response {
	id, _ := req.Args["id"].(string)
	if id == "" {
		return Response{Success: false, Error: "missing id"}
	}
	if err := s.daemon.CancelTicket(id); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	return Response{Success: true, Data: map[string]interface{}{"closed": id}}
}

func (s *Server) cmdCancel(req *Request) Response {
	id, _ := req.Args["id"].(string)
	if id == "" {
		return Response{Success: false, Error: "missing id"}
	}
	if err := s.daemon.CancelTicket(id); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	return Response{Success: true}
}

// validateTicket enforces the 5 hard constraints
func validateTicket(t *Ticket) error {
	if t.Goal == "" || len(t.Goal) <= 10 {
		return fmt.Errorf("goal must be non-empty and > 10 chars")
	}
	if len(t.Acceptance) == 0 {
		return fmt.Errorf("acceptance must have at least 1 item")
	}
	hasIntent := false
	if t.Context != nil {
		if _, ok := t.Context["user_intent"]; ok {
			hasIntent = true
		}
		if facts, ok := t.Context["known_facts"]; ok {
			if arr, ok := facts.([]interface{}); ok && len(arr) > 0 {
				hasIntent = true
			}
		}
	}
	if !hasIntent {
		return fmt.Errorf("context must contain user_intent or non-empty known_facts")
	}
	if t.MaxDepth <= 0 || t.MaxDepth > 3 {
		t.MaxDepth = 3
	}
	if t.Depth < 1 {
		t.Depth = 1
	}
	if t.Depth > t.MaxDepth {
		return fmt.Errorf("depth %d exceeds max_depth %d", t.Depth, t.MaxDepth)
	}
	if t.SessionID == "" {
		t.SessionID = t.ID
	}
	return nil
}
