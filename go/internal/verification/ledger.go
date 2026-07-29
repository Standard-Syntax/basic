package verification

import (
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

func verificationRequestDigest(request Request) (string, error) {
	implementation, err := proto.MarshalOptions{Deterministic: true}.Marshal(request.Implementation)
	if err != nil {
		return "", fmt.Errorf("serialize verification implementation: %w", err)
	}
	bindings, err := json.Marshal(struct {
		VerificationID        string
		VerificationTimestamp string
		ExecutionURI          string
		ExecutionDigest       string
		CandidateCommit       string
		ExpectedTaskRevision  uint64
		Requirements          []AcceptanceRequirement
	}{
		VerificationID:        request.VerificationID,
		VerificationTimestamp: request.VerificationTimestamp.UTC().Format(time.RFC3339Nano),
		ExecutionURI:          request.ExecutionArtifact.URI, ExecutionDigest: request.ExecutionArtifact.Digest,
		CandidateCommit:      request.CandidateCommit,
		ExpectedTaskRevision: request.ExpectedTaskRevision,
		Requirements:         request.Requirements,
	})
	if err != nil {
		return "", fmt.Errorf("serialize verification bindings: %w", err)
	}
	hash := sha256.New()
	for _, value := range [][]byte{implementation, bindings} {
		if err := binary.Write(hash, binary.BigEndian, uint64(len(value))); err != nil {
			return "", fmt.Errorf("hash verification request: %w", err)
		}
		_, _ = hash.Write(value)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type memoryVerificationRecord struct {
	digest        string
	owner         string
	reservedUntil time.Time
	finalTime     *time.Time
	evidence      *VerificationEvidence
	result        *Result
}

type MemoryVerificationLedger struct {
	mu      sync.Mutex
	records map[string]memoryVerificationRecord
	now     func() time.Time
}

func NewMemoryVerificationLedger() *MemoryVerificationLedger {
	return &MemoryVerificationLedger{
		records: make(map[string]memoryVerificationRecord), now: time.Now,
	}
}

func (l *MemoryVerificationLedger) Begin(
	ctx context.Context, start VerificationStart,
) (VerificationHandle, error) {
	for {
		l.mu.Lock()
		record, exists := l.records[start.VerificationID]
		if exists && record.digest != start.RequestDigest {
			l.mu.Unlock()
			return nil, ErrVerificationConflict
		}
		if !exists || record.evidence == nil && !record.reservedUntil.After(l.now()) {
			owner := uuid.NewString()
			record.digest = start.RequestDigest
			record.owner = owner
			record.reservedUntil = l.now().Add(start.ReservationTTL)
			l.records[start.VerificationID] = record
			l.mu.Unlock()
			return &memoryVerificationHandle{
				ledger: l, verificationID: start.VerificationID,
				digest: start.RequestDigest, owner: owner,
			}, nil
		}
		if record.result != nil {
			result := cloneResult(*record.result)
			l.mu.Unlock()
			return &memoryVerificationHandle{replay: &result}, nil
		}
		if record.evidence != nil {
			evidence := cloneEvidence(*record.evidence)
			l.mu.Unlock()
			return &memoryVerificationHandle{
				ledger: l, verificationID: start.VerificationID,
				digest: start.RequestDigest, owner: record.owner, evidence: &evidence,
			}, nil
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

type memoryVerificationHandle struct {
	ledger         *MemoryVerificationLedger
	verificationID string
	digest         string
	owner          string
	replay         *Result
	evidence       *VerificationEvidence
}

func (h *memoryVerificationHandle) Replay() (Result, bool) {
	if h.replay == nil {
		return Result{}, false
	}
	return cloneResult(*h.replay), true
}

func (h *memoryVerificationHandle) Evidence() (VerificationEvidence, bool) {
	if h.evidence == nil {
		return VerificationEvidence{}, false
	}
	return cloneEvidence(*h.evidence), true
}

func (h *memoryVerificationHandle) SaveEvidence(
	_ context.Context, evidence VerificationEvidence,
) error {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	record, ok := h.ledger.records[h.verificationID]
	if !ok || record.digest != h.digest || record.owner != h.owner || record.evidence != nil {
		return ErrVerificationConflict
	}
	stored := cloneEvidence(evidence)
	record.evidence = &stored
	h.ledger.records[h.verificationID] = record
	h.evidence = &stored
	return nil
}

func (h *memoryVerificationHandle) FinalTransitionTime(
	_ context.Context, value time.Time,
) (time.Time, error) {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	record, ok := h.ledger.records[h.verificationID]
	if !ok || record.digest != h.digest || record.owner != h.owner ||
		record.evidence == nil || record.result != nil {
		return time.Time{}, ErrVerificationConflict
	}
	if record.finalTime == nil {
		mapped := value.UTC()
		record.finalTime = &mapped
		h.ledger.records[h.verificationID] = record
	}
	return *record.finalTime, nil
}

func (h *memoryVerificationHandle) Complete(_ context.Context, result Result) error {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	record, ok := h.ledger.records[h.verificationID]
	if !ok || record.digest != h.digest || record.owner != h.owner ||
		record.evidence == nil || record.result != nil || record.finalTime == nil {
		return ErrVerificationConflict
	}
	stored := cloneResult(result)
	record.result = &stored
	h.ledger.records[h.verificationID] = record
	return nil
}

func cloneResult(value Result) Result {
	encoded, _ := json.Marshal(value)
	var cloned Result
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}

func cloneEvidence(value VerificationEvidence) VerificationEvidence {
	encoded, _ := json.Marshal(value)
	var cloned VerificationEvidence
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}
