package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/jackc/pgx/v5/pgxpool"
)

const maximumSupportDiagnostics = 1000

var safeDiagnosticField = regexp.MustCompile(`^[A-Za-z0-9_.\[\]-]{1,128}$`)

// ReasoningDiagnostic is the bounded, secret-safe projection of one provider
// invocation. It deliberately excludes prompts, responses, provider request
// identifiers, artifact locations, and free-form rejection messages.
type ReasoningDiagnostic struct {
	Stage            string               `json:"stage"`
	Attempt          uint32               `json:"attempt"`
	Provider         *string              `json:"provider,omitempty"`
	Model            *string              `json:"model,omitempty"`
	FinalStatus      *string              `json:"final_status,omitempty"`
	InputTokens      uint64               `json:"input_tokens"`
	OutputTokens     uint64               `json:"output_tokens"`
	ProviderRequests uint32               `json:"provider_requests"`
	Rejection        *RejectionDiagnostic `json:"rejection,omitempty"`
}

type RejectionDiagnostic struct {
	Code      int32    `json:"code"`
	Category  string   `json:"category"`
	Fields    []string `json:"fields,omitempty"`
	Retryable bool     `json:"retryable"`
}

type SupportReader interface {
	ReasoningDiagnostics(context.Context, string) ([]ReasoningDiagnostic, error)
}

type PostgresSupportReader struct{ pool *pgxpool.Pool }

func NewPostgresSupportReader(pool *pgxpool.Pool) (*PostgresSupportReader, error) {
	if pool == nil {
		return nil, errors.New("PostgreSQL pool is required")
	}
	return &PostgresSupportReader{pool: pool}, nil
}

func (r *PostgresSupportReader) ReasoningDiagnostics(
	ctx context.Context, runID string,
) ([]ReasoningDiagnostic, error) {
	rows, err := r.pool.Query(ctx, `SELECT stage,attempt,provider,model,final_status,
		input_tokens,output_tokens,provider_requests,rejection_code,rejection_details,
		rejection_retryable FROM reasoning_invocations WHERE run_id=$1
		ORDER BY started_at,request_id LIMIT $2`, runID, maximumSupportDiagnostics+1)
	if err != nil {
		return nil, fmt.Errorf("read support diagnostics: %w", err)
	}
	defer rows.Close()
	result := make([]ReasoningDiagnostic, 0)
	for rows.Next() {
		var value ReasoningDiagnostic
		var code *int32
		var details []byte
		var retryable *bool
		if err := rows.Scan(
			&value.Stage, &value.Attempt, &value.Provider, &value.Model, &value.FinalStatus,
			&value.InputTokens, &value.OutputTokens, &value.ProviderRequests, &code, &details,
			&retryable,
		); err != nil {
			return nil, fmt.Errorf("scan support diagnostic: %w", err)
		}
		value.Rejection, err = storedRejectionDiagnostic(code, details, retryable)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate support diagnostics: %w", err)
	}
	if len(result) > maximumSupportDiagnostics {
		return nil, errors.New("support diagnostics exceed row limit")
	}
	return result, nil
}

func storedRejectionDiagnostic(
	code *int32, details []byte, retryable *bool,
) (*RejectionDiagnostic, error) {
	if code == nil && retryable == nil {
		return nil, nil
	}
	if code == nil || retryable == nil {
		return nil, errors.New("stored rejection state is incomplete")
	}
	fields, err := rejectionFields(details)
	if err != nil {
		return nil, err
	}
	return &RejectionDiagnostic{
		Code: *code, Category: rejectionCategory(*code), Fields: fields,
		Retryable: *retryable,
	}, nil
}

func rejectionFields(body []byte) ([]string, error) {
	if len(body) == 0 {
		return nil, nil
	}
	var details []struct {
		Field string `json:"field"`
	}
	if err := json.Unmarshal(body, &details); err != nil {
		return nil, errors.New("stored rejection details are invalid")
	}
	if len(details) > 32 {
		return nil, errors.New("stored rejection details exceed limit")
	}
	fields := make([]string, 0, len(details))
	for _, detail := range details {
		if safeDiagnosticField.MatchString(detail.Field) {
			fields = append(fields, detail.Field)
		}
	}
	slices.Sort(fields)
	return slices.Compact(fields), nil
}

func rejectionCategory(code int32) string {
	switch code {
	case 1:
		return "schema_invalid"
	case 2:
		return "policy_denied"
	case 3:
		return "stale_context"
	case 4:
		return "verification_failed"
	case 5:
		return "review_failed"
	default:
		return "unknown"
	}
}
