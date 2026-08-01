package verification

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Standard-Syntax/basic/go/internal/dockerengine"
)

type inspectEngine struct{ id string }

func (e inspectEngine) ImageID(context.Context, string) (string, error) { return e.id, nil }
func (inspectEngine) Run(context.Context, dockerengine.RunRequest, io.Reader, io.Writer) error {
	return nil
}
func (inspectEngine) Remove(context.Context, string) error { return nil }

func TestDockerExecutorRejectsMutableOrMalformedImageIdentity(t *testing.T) {
	executor := DockerCheckExecutor{Image: "verification:test", UID: 1000, GID: 1000, Engine: inspectEngine{id: "not-an-image-id"}}
	if _, err := executor.ImageID(t.Context()); err == nil {
		t.Fatal("malformed image identity accepted")
	}
	executor.Engine = inspectEngine{id: "sha256:" + strings.Repeat("a", 64)}
	if id, err := executor.ImageID(t.Context()); err != nil || id == "" {
		t.Fatalf("immutable image identity = %q, %v", id, err)
	}
}

func TestBoundedBufferRetainsPrefixAndSignalsOverflow(t *testing.T) {
	buffer := dockerengine.NewBoundedWriter(4)
	if count, err := buffer.Write([]byte("abcdef")); err != nil || count != 6 {
		t.Fatalf("write = %d, %v", count, err)
	}
	if string(buffer.Bytes()) != "abcd" || !buffer.Overflow() {
		t.Fatalf("buffer = %q overflow=%v", buffer.Bytes(), buffer.Overflow())
	}
}
