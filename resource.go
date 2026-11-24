package main

import (
	"fmt"
	"sync"
)

// ResourceManager tracks resource leases: which ticket holds which resource key.
// The kernel only enforces "same key, one exclusive holder at a time".
// It does not know what a worktree or port actually is — that's the adapter's job.
type ResourceManager struct {
	mu sync.Mutex
	// key -> holder ticketID. Presence of a key means it is exclusively held.
	exclusive map[string]string
	// key -> set of ticketIDs sharing it.
	shared map[string]map[string]bool
}

// NewResourceManager creates an empty manager
func NewResourceManager() *ResourceManager {
	return &ResourceManager{
		exclusive: make(map[string]string),
		shared:    make(map[string]map[string]bool),
	}
}

// Acquire tries to acquire all resources for a ticket.
// Returns nil on success, or an error describing the first conflict.
// Acquire is atomic across the whole set: either all acquired or none.
func (rm *ResourceManager) Acquire(ticketID string, reqs []ResourceReq) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	// Check conflicts first
	for _, r := range reqs {
		mode := r.Mode
		if mode == "" {
			mode = "exclusive"
		}
		if mode == "exclusive" {
			if holder, taken := rm.exclusive[r.Key]; taken && holder != ticketID {
				return fmt.Errorf("resource %s (kind=%s) is held by %s", r.Key, r.Kind, holder)
			}
			if sharers, shared := rm.shared[r.Key]; shared && len(sharers) > 0 {
				if !(len(sharers) == 1 && sharers[ticketID]) {
					return fmt.Errorf("resource %s (kind=%s) is shared by others", r.Key, r.Kind)
				}
			}
		} else { // shared
			if holder, taken := rm.exclusive[r.Key]; taken && holder != ticketID {
				return fmt.Errorf("resource %s (kind=%s) is exclusively held by %s", r.Key, r.Kind, holder)
			}
		}
	}

	// No conflicts: acquire all
	for _, r := range reqs {
		mode := r.Mode
		if mode == "" {
			mode = "exclusive"
		}
		if mode == "exclusive" {
			rm.exclusive[r.Key] = ticketID
			delete(rm.shared, r.Key) // upgrade from shared if present
		} else {
			if rm.shared[r.Key] == nil {
				rm.shared[r.Key] = make(map[string]bool)
			}
			rm.shared[r.Key][ticketID] = true
		}
	}
	return nil
}

// Release releases all resources held by a ticket
func (rm *ResourceManager) Release(ticketID string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	for key, holder := range rm.exclusive {
		if holder == ticketID {
			delete(rm.exclusive, key)
		}
	}
	for key, sharers := range rm.shared {
		delete(sharers, ticketID)
		if len(sharers) == 0 {
			delete(rm.shared, key)
		}
	}
}

// ReleaseKeys releases specific resource keys held by a ticket (used at close)
func (rm *ResourceManager) ReleaseKeys(ticketID string, keys []string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	for _, key := range keys {
		if holder, ok := rm.exclusive[key]; ok && holder == ticketID {
			delete(rm.exclusive, key)
		}
		if sharers, ok := rm.shared[key]; ok {
			delete(sharers, ticketID)
			if len(sharers) == 0 {
				delete(rm.shared, key)
			}
		}
	}
}

// Holder returns the ticket holding an exclusive resource, or ""
func (rm *ResourceManager) Holder(key string) string {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return rm.exclusive[key]
}

// Snapshot returns a copy of current leases (for inspection / reconcile)
func (rm *ResourceManager) Snapshot() (exclusive map[string]string, shared map[string][]string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	exclusive = make(map[string]string, len(rm.exclusive))
	for k, v := range rm.exclusive {
		exclusive[k] = v
	}
	shared = make(map[string][]string, len(rm.shared))
	for k, set := range rm.shared {
		for id := range set {
			shared[k] = append(shared[k], id)
		}
	}
	return
}
