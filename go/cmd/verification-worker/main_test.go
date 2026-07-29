package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestWorkerRejectsUnapprovedCommand(t *testing.T) {
	input := `{"command_reference":"model-command","argv":["sh","-c","true"],` +
		`"timeout_nanoseconds":1000000000,"output_bytes":1024}`
	var output bytes.Buffer
	if err := run(strings.NewReader(input), &output); err == nil {
		t.Fatal("unapproved command executed")
	}
}

func TestWorkerRejectsUnknownFieldsAndTrailers(t *testing.T) {
	cases := []string{
		`{"command_reference":"make-check-v1","argv":["make","check"],` +
			`"timeout_nanoseconds":1000000000,"output_bytes":1024,"extra":true}`,
		`{"command_reference":"make-check-v1","argv":["make","check"],` +
			`"timeout_nanoseconds":1000000000,"output_bytes":1024}{}`,
	}
	for _, input := range cases {
		if _, err := decode(strings.NewReader(input)); err == nil {
			t.Fatalf("invalid input accepted: %s", input)
		}
	}
}

func TestWorkerBoundedBuffer(t *testing.T) {
	buffer := boundedBuffer{limit: 3}
	_, _ = buffer.Write([]byte("abcdef"))
	if got := buffer.buffer.String(); got != "abc" || !buffer.overflow {
		t.Fatalf("buffer = %q overflow=%v", got, buffer.overflow)
	}
}
