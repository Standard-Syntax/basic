package approval

import (
	"context"
	"sync"
)

type memoryApprovalEntry struct {
	start    ApprovalStart
	decision *DecisionCheckpoint
	result   *Result
	wait     chan struct{}
}

type MemoryApprovalLedger struct {
	mu      sync.Mutex
	entries map[string]*memoryApprovalEntry
}

func NewMemoryApprovalLedger() *MemoryApprovalLedger {
	return &MemoryApprovalLedger{entries: make(map[string]*memoryApprovalEntry)}
}

func (l *MemoryApprovalLedger) Begin(
	ctx context.Context, start ApprovalStart,
) (ApprovalHandle, error) {
	for {
		l.mu.Lock()
		entry, exists := l.entries[start.ApprovalID]
		if !exists {
			entry = &memoryApprovalEntry{start: start, wait: make(chan struct{})}
			l.entries[start.ApprovalID] = entry
			l.mu.Unlock()
			return &memoryApprovalHandle{ledger: l, entry: entry, owner: true}, nil
		}
		if entry.start.RequestDigest != start.RequestDigest {
			l.mu.Unlock()
			return nil, ErrApprovalConflict
		}
		if entry.decision != nil || entry.result != nil {
			l.mu.Unlock()
			return &memoryApprovalHandle{ledger: l, entry: entry}, nil
		}
		wait := entry.wait
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wait:
		}
	}
}

type memoryApprovalHandle struct {
	ledger *MemoryApprovalLedger
	entry  *memoryApprovalEntry
	owner  bool
}

func (h *memoryApprovalHandle) Replay() (Result, bool) {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	if h.entry.result == nil {
		return Result{}, false
	}
	return cloneResult(*h.entry.result), true
}

func (h *memoryApprovalHandle) Decision() (DecisionCheckpoint, bool) {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	if h.entry.decision == nil {
		return DecisionCheckpoint{}, false
	}
	return cloneCheckpoint(*h.entry.decision), true
}

func (h *memoryApprovalHandle) SaveDecision(
	_ context.Context, decision DecisionCheckpoint,
) error {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	if !h.owner || h.entry.decision != nil || h.entry.result != nil {
		return ErrApprovalState
	}
	value := cloneCheckpoint(decision)
	h.entry.decision = &value
	h.owner = false
	close(h.entry.wait)
	h.entry.wait = make(chan struct{})
	return nil
}

func (h *memoryApprovalHandle) Complete(_ context.Context, result Result) error {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	if h.entry.decision == nil {
		return ErrApprovalState
	}
	if h.entry.result != nil {
		if h.entry.result.ApprovalArtifact.Equal(result.ApprovalArtifact) &&
			h.entry.result.Decision == result.Decision {
			return nil
		}
		return ErrApprovalConflict
	}
	value := cloneResult(result)
	h.entry.result = &value
	return nil
}

func (h *memoryApprovalHandle) Rollback(_ context.Context) error {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	if !h.owner || h.entry.decision != nil || h.entry.result != nil {
		return nil
	}
	delete(h.ledger.entries, h.entry.start.ApprovalID)
	h.owner = false
	close(h.entry.wait)
	return nil
}

func cloneCheckpoint(value DecisionCheckpoint) DecisionCheckpoint {
	value.Result = cloneResult(value.Result)
	return value
}

func cloneResult(value Result) Result {
	value.RiskReasons = append([]string(nil), value.RiskReasons...)
	return value
}
