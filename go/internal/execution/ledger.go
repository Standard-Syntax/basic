package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func executionRequestDigest(request Request) (string, error) {
	requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(request.Implementation)
	if err != nil {
		return "", fmt.Errorf("serialize execution request: %w", err)
	}
	proposalBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(request.Proposal)
	if err != nil {
		return "", fmt.Errorf("serialize execution proposal: %w", err)
	}
	bindings, err := json.Marshal(struct {
		ExecutionID          string
		ExecutionTimestamp   string
		ProposalURI          string
		ProposalDigest       string
		LeaseID              string
		LeaseOwnerID         string
		LeaseExpiry          string
		FencingToken         uint32
		ExpectedTaskRevision uint64
	}{
		ExecutionID:        request.ExecutionID,
		ExecutionTimestamp: request.ExecutionTimestamp.UTC().Format(time.RFC3339Nano),
		ProposalURI:        request.ProposalArtifact.URI,
		ProposalDigest:     request.ProposalArtifact.Digest,
		LeaseID:            request.Lease.ID, LeaseOwnerID: request.Lease.OwnerID,
		LeaseExpiry:          request.Lease.ExpiresAt.UTC().Format(time.RFC3339Nano),
		FencingToken:         request.Lease.FencingToken,
		ExpectedTaskRevision: request.ExpectedTaskRevision,
	})
	if err != nil {
		return "", fmt.Errorf("serialize execution bindings: %w", err)
	}
	hash := sha256.New()
	for _, value := range [][]byte{requestBytes, proposalBytes, bindings} {
		if err := binary.Write(hash, binary.BigEndian, uint64(len(value))); err != nil {
			return "", fmt.Errorf("hash execution request: %w", err)
		}
		_, _ = hash.Write(value)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type memoryExecutionRecord struct {
	digest        string
	owner         string
	reservedUntil time.Time
	finalTime     *time.Time
	result        *Result
}

type MemoryExecutionLedger struct {
	mu      sync.Mutex
	records map[string]memoryExecutionRecord
	now     func() time.Time
}

func NewMemoryExecutionLedger() *MemoryExecutionLedger {
	return &MemoryExecutionLedger{
		records: make(map[string]memoryExecutionRecord), now: time.Now,
	}
}

func (l *MemoryExecutionLedger) Begin(
	ctx context.Context, start ExecutionStart,
) (ExecutionHandle, error) {
	for {
		l.mu.Lock()
		record, exists := l.records[start.ExecutionID]
		if exists && record.digest != start.RequestDigest {
			l.mu.Unlock()
			return nil, ErrExecutionConflict
		}
		if !exists || record.result == nil && !record.reservedUntil.After(l.now()) {
			owner := uuid.NewString()
			l.records[start.ExecutionID] = memoryExecutionRecord{
				digest: start.RequestDigest, owner: owner,
				reservedUntil: l.now().Add(start.ReservationTTL),
			}
			l.mu.Unlock()
			return &memoryExecutionHandle{
				ledger: l, executionID: start.ExecutionID,
				digest: start.RequestDigest, owner: owner,
			}, nil
		}
		if record.result != nil {
			result := cloneResult(*record.result)
			l.mu.Unlock()
			return &memoryExecutionHandle{replay: &result}, nil
		}
		l.mu.Unlock()
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

type memoryExecutionHandle struct {
	ledger      *MemoryExecutionLedger
	executionID string
	digest      string
	owner       string
	replay      *Result
}

func (h *memoryExecutionHandle) Replay() (Result, bool) {
	if h.replay == nil {
		return Result{}, false
	}
	return cloneResult(*h.replay), true
}

func (h *memoryExecutionHandle) Abandon(_ context.Context) error {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	record, ok := h.ledger.records[h.executionID]
	if !ok || record.digest != h.digest || record.owner != h.owner || record.result != nil {
		return ErrExecutionConflict
	}
	record.reservedUntil = h.ledger.now()
	h.ledger.records[h.executionID] = record
	return nil
}

func (h *memoryExecutionHandle) FinalTransitionTime(
	_ context.Context, value time.Time,
) (time.Time, error) {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	record, ok := h.ledger.records[h.executionID]
	if !ok || record.digest != h.digest || record.owner != h.owner || record.result != nil {
		return time.Time{}, ErrExecutionConflict
	}
	if record.finalTime == nil {
		mapped := value.UTC()
		record.finalTime = &mapped
		h.ledger.records[h.executionID] = record
	}
	return *record.finalTime, nil
}

func (h *memoryExecutionHandle) Complete(_ context.Context, result Result) error {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	record, ok := h.ledger.records[h.executionID]
	if !ok || record.digest != h.digest || record.owner != h.owner ||
		record.result != nil || record.finalTime == nil {
		return ErrExecutionConflict
	}
	stored := cloneResult(result)
	record.result = &stored
	h.ledger.records[h.executionID] = record
	return nil
}

func cloneResult(value Result) Result {
	encoded, _ := json.Marshal(value)
	var cloned Result
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}

func resultBytes(value Result) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}
