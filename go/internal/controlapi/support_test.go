package controlapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRejectionFieldsDropsFreeFormMessagesAndUnsafeFields(t *testing.T) {
	const secret = "RAW_PROVIDER_SECRET_SENTINEL"
	fields, err := rejectionFields([]byte(`[
		{"field":"provider_response","message":"` + secret + `"},
		{"field":"unsafe field","message":"also secret"},
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
		strings.Contains(string(encoded), secret) {
		t.Fatalf("diagnostic=%s", encoded)
	}
}
