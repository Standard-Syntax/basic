package publication

import (
	"context"
	"sync"
)

type memoryPublicationEntry struct {
	start  PublicationStart
	branch *BranchCheckpoint
	pull   *PullRequestCheckpoint
	result *Result
	wait   chan struct{}
}

type MemoryPublicationLedger struct {
	mu      sync.Mutex
	entries map[string]*memoryPublicationEntry
}

func NewMemoryPublicationLedger() *MemoryPublicationLedger {
	return &MemoryPublicationLedger{entries: make(map[string]*memoryPublicationEntry)}
}

func (l *MemoryPublicationLedger) Begin(
	ctx context.Context, start PublicationStart,
) (PublicationHandle, error) {
	for {
		l.mu.Lock()
		entry, exists := l.entries[start.PublicationID]
		if !exists {
			entry = &memoryPublicationEntry{start: start, wait: make(chan struct{})}
			l.entries[start.PublicationID] = entry
			l.mu.Unlock()
			return &memoryPublicationHandle{ledger: l, entry: entry, owner: true}, nil
		}
		if entry.start.RequestDigest != start.RequestDigest {
			l.mu.Unlock()
			return nil, ErrPublicationConflict
		}
		if entry.branch != nil || entry.pull != nil || entry.result != nil {
			l.mu.Unlock()
			return &memoryPublicationHandle{ledger: l, entry: entry}, nil
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

type memoryPublicationHandle struct {
	ledger *MemoryPublicationLedger
	entry  *memoryPublicationEntry
	owner  bool
}

func (h *memoryPublicationHandle) Replay() (Result, bool) {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	if h.entry.result == nil {
		return Result{}, false
	}
	value := *h.entry.result
	return value, true
}

func (h *memoryPublicationHandle) Branch() (BranchCheckpoint, bool) {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	if h.entry.branch == nil {
		return BranchCheckpoint{}, false
	}
	return *h.entry.branch, true
}

func (h *memoryPublicationHandle) SaveBranch(
	_ context.Context, checkpoint BranchCheckpoint,
) error {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	if h.entry.branch != nil || h.entry.result != nil {
		return ErrPublicationState
	}
	value := checkpoint
	h.entry.branch = &value
	h.owner = false
	close(h.entry.wait)
	h.entry.wait = make(chan struct{})
	return nil
}

func (h *memoryPublicationHandle) PullRequest() (PullRequestCheckpoint, bool) {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	if h.entry.pull == nil {
		return PullRequestCheckpoint{}, false
	}
	return *h.entry.pull, true
}

func (h *memoryPublicationHandle) SavePullRequest(
	_ context.Context, checkpoint PullRequestCheckpoint,
) error {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	if h.entry.branch == nil || h.entry.pull != nil || h.entry.result != nil {
		return ErrPublicationState
	}
	value := checkpoint
	h.entry.pull = &value
	return nil
}

func (h *memoryPublicationHandle) Complete(_ context.Context, result Result) error {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	if h.entry.pull == nil {
		return ErrPublicationState
	}
	if h.entry.result != nil {
		if h.entry.result.PublicationArtifact.Equal(result.PublicationArtifact) {
			return nil
		}
		return ErrPublicationConflict
	}
	value := result
	h.entry.result = &value
	return nil
}

func (h *memoryPublicationHandle) Rollback(_ context.Context) error {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	if !h.owner || h.entry.branch != nil || h.entry.pull != nil || h.entry.result != nil {
		return nil
	}
	delete(h.ledger.entries, h.entry.start.PublicationID)
	h.owner = false
	close(h.entry.wait)
	return nil
}
