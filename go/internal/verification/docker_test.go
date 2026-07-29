package verification

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDockerExecutorRejectsMutableOrMalformedImageIdentity(t *testing.T) {
	bin := t.TempDir()
	docker := filepath.Join(bin, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nprintf 'not-an-image-id\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	executor := DockerCheckExecutor{Image: "verification:test", UID: 1000, GID: 1000}
	if _, err := executor.ImageID(t.Context()); err == nil {
		t.Fatal("malformed image identity accepted")
	}
}

func TestBoundedBufferRetainsPrefixAndSignalsOverflow(t *testing.T) {
	buffer := boundedBuffer{limit: 4}
	if count, err := buffer.Write([]byte("abcdef")); err != nil || count != 6 {
		t.Fatalf("write = %d, %v", count, err)
	}
	if string(buffer.Bytes()) != "abcd" || !buffer.overflow {
		t.Fatalf("buffer = %q overflow=%v", buffer.Bytes(), buffer.overflow)
	}
}
