package contracts

import (
	"errors"
	"fmt"

	reasoningv1 "github.com/Standard-Syntax/basic/go/gen/harness/reasoning/v1"
)

// ValidationFailure is a policy rejection produced while mapping untrusted
// reasoning transport values into domain values.
type ValidationFailure struct {
	Code              reasoningv1.RejectionCode
	Field             string
	Message           string
	Kind              string
	JSONOffset        int64
	UnknownFields     []string
	ContentBlockTypes []string
}

func (e *ValidationFailure) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

func validationFailure(
	code reasoningv1.RejectionCode, field, message string,
) error {
	return &ValidationFailure{Code: code, Field: field, Message: message}
}

// ValidationCode returns the stable rejection code carried by err.
func ValidationCode(err error) (reasoningv1.RejectionCode, bool) {
	var failure *ValidationFailure
	if !errors.As(err, &failure) {
		return reasoningv1.RejectionCode_REJECTION_CODE_UNSPECIFIED, false
	}
	return failure.Code, true
}
