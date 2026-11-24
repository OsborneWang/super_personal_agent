package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event represents a single ledger event
type Event struct {
	V      int             `json:"v"`
	Seq    int64           `json:"seq"`
	At     time.Time       `json:"at"`
	Ticket string          `json:"ticket,omitempty"`
	Kind   string          `json:"kind"`
	By     Actor           `json:"by"`
	Data   json.RawMessage `json:"data"`
}

type Actor struct {
	Actor string `json:"actor"` // user|agentd|router|worker|timer
	Run   string `json:"run,omitempty"`
	PID   int    `json:"pid,omitempty"`
	Who   string `json:"who,omitempty"`
}

// Ledger is the append-only event log
type Ledger struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	writer   *bufio.Writer
	seq      int64
	highWater int64
}

// NewLedger opens or creates a ledger file
func NewLedger(path string) (*Ledger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create ledger dir: %w", err)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}

	l := &Ledger{
		path:   path,
		file:   file,
		writer: bufio.NewWriter(file),
		seq:    0,
	}

	// Recover high water mark by scanning existing file
	if err := l.recoverHighWater(); err != nil {
		file.Close()
		return nil, fmt.Errorf("recover high water: %w", err)
	}

	return l, nil
}

// recoverHighWater scans the file to find the highest seq number
func (l *Ledger) recoverHighWater() error {
	file, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Empty ledger
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			// Incomplete line at end - truncate
			continue
		}

		if event.Seq > l.highWater {
			l.highWater = event.Seq
		}
	}

	return scanner.Err()
}

// Append adds an event to the ledger
func (l *Ledger) Append(kind string, ticket string, by Actor, data interface{}) (*Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.seq++
	event := Event{
		V:      1,
		Seq:    l.seq,
		At:     time.Now(),
		Ticket: ticket,
		Kind:   kind,
		By:     by,
	}

	if data != nil {
		jsonData, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("marshal data: %w", err)
		}
		event.Data = jsonData
	}

	line, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("marshal event: %w", err)
	}

	if _, err := l.writer.Write(line); err != nil {
		return nil, fmt.Errorf("write event: %w", err)
	}

	if err := l.writer.WriteByte('\n'); err != nil {
		return nil, fmt.Errorf("write newline: %w", err)
	}

	// Flush for durability
	if err := l.writer.Flush(); err != nil {
		return nil, fmt.Errorf("flush: %w", err)
	}

	if event.Seq > l.highWater {
		l.highWater = event.Seq
	}

	return &event, nil
}

// Close closes the ledger
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.writer.Flush(); err != nil {
		return err
	}
	return l.file.Close()
}

// Replay reads all events from the ledger
func (l *Ledger) Replay() ([]*Event, error) {
	file, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return []*Event{}, nil
		}
		return nil, err
	}
	defer file.Close()

	var events []*Event
	scanner := bufio.NewScanner(file)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			// Incomplete line at end - truncate file here
			if err := l.truncateAtLine(lineNum - 1); err != nil {
				return nil, fmt.Errorf("truncate incomplete line: %w", err)
			}
			break
		}

		events = append(events, &event)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return events, nil
}

// truncateAtLine truncates the file to keep only the first N lines
func (l *Ledger) truncateAtLine(keepLines int) error {
	file, err := os.Open(l.path)
	if err != nil {
		return err
	}

	var lines [][]byte
	scanner := bufio.NewScanner(file)
	for scanner.Scan() && len(lines) < keepLines {
		line := make([]byte, len(scanner.Bytes()))
		copy(line, scanner.Bytes())
		lines = append(lines, line)
	}
	file.Close()

	if err := scanner.Err(); err != nil {
		return err
	}

	// Rewrite file with only valid lines
	tmpPath := l.path + ".tmp"
	tmpFile, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	writer := bufio.NewWriter(tmpFile)
	for _, line := range lines {
		writer.Write(line)
		writer.WriteByte('\n')
	}
	writer.Flush()
	tmpFile.Close()

	// Atomic replace
	return os.Rename(tmpPath, l.path)
}

// HighWater returns the highest seq number seen
func (l *Ledger) HighWater() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.highWater
}
