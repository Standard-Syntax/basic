package controlapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRejectionFieldsDropsFreeFormMessagesAndUnsafeFields(t *testing.T) {
	const rawValue = "UNTRUSTED_RESPONSE_VALUE_123"
	fields, err := rejectionFields([]byte(`[
		{"field":"provider_response","message":"` + rawValue + `"},
		{"field":"unsafe field","message":"untrusted value"},
		{"field":"provider_response","message":"duplicate"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(RejectionDiagnostic{
		Code: 1, Category: rejectionCategory(1), Fields: fields,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"code":1,"category":"schema_invalid","fields":["provider_response"],"retryable":false}` ||
		strings.Contains(string(encoded), rawValue) {
		t.Fatalf("diagnostic=%s", encoded)
	}
}

func TestStoredRejectionDiagnosticRejectsPartialState(t *testing.T) {
	code, retryable := int32(1), false
	if value, err := storedRejectionDiagnostic(nil, nil, nil); err != nil || value != nil {
		t.Fatalf("absent rejection=%+v err=%v", value, err)
	}
	value, err := storedRejectionDiagnostic(&code, []byte(`[{"field":"proposal"}]`), &retryable)
	if err != nil || value == nil || value.Code != code || len(value.Fields) != 1 {
		t.Fatalf("complete rejection=%+v err=%v", value, err)
	}
	for _, test := range []struct {
		name      string
		code      *int32
		retryable *bool
	}{
		{name: "missing retryable", code: &code},
		{name: "missing code", retryable: &retryable},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := storedRejectionDiagnostic(test.code, nil, test.retryable)
			if err == nil || err.Error() != "stored rejection state is incomplete" {
				t.Fatalf("partial rejection state error=%v", err)
			}
		})
	}
}
