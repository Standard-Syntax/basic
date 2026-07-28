package registry

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/manifest"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../../../tests/contracts/v1/manifest/implementation.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestInvalidInputsFailBeforeDatabaseAccess(t *testing.T) {
	registry := New(nil)
	if _, err := registry.Register(t.Context(), []byte(`{"unknown":true}`)); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("Register error = %v", err)
	}
	if _, err := registry.Get(t.Context(), "Invalid Name", "1.0.0"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Get error = %v", err)
	}
	if _, err := registry.Get(t.Context(), "valid-name", "v1"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Get version error = %v", err)
	}
	if _, err := registry.GetByDigest(t.Context(), "ABC"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("GetByDigest error = %v", err)
	}
}

func TestValidateRecordDetectsDriftAndOwnsBytes(t *testing.T) {
	value, canonical, digest, err := manifest.Read(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	input := append([]byte(nil), canonical...)
	record, err := validateRecord(Record{
		CanonicalJSON: input, Digest: digest, RegisteredAt: time.Now(),
	}, value.Agent.Name, value.Agent.Version)
	if err != nil {
		t.Fatal(err)
	}
	record.CanonicalJSON[0] = ' '
	if input[0] != '{' {
		t.Fatal("returned canonical bytes alias persisted input")
	}

	cases := []Record{
		{CanonicalJSON: []byte(`{}`), Digest: digest},
		{CanonicalJSON: canonical, Digest: "0" + digest[1:]},
	}
	for _, candidate := range cases {
		if _, err := validateRecord(
			candidate, value.Agent.Name, value.Agent.Version,
		); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("validateRecord error = %v", err)
		}
	}
	if _, err := validateRecord(
		Record{CanonicalJSON: canonical, Digest: digest}, "other-agent", value.Agent.Version,
	); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("identity drift error = %v", err)
	}
}
