package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/beta"
	"github.com/Standard-Syntax/basic/go/internal/release"
)

func TestReadinessCLIExitContractAndRedaction(t *testing.T) {
	originalLoad, originalVerify := loadManifest, verifyManifest
	t.Cleanup(func() { loadManifest, verifyManifest = originalLoad, originalVerify })
	tests := []struct {
		name       string
		load       func(string) (beta.ReleaseManifest, error)
		verify     func(context.Context, *beta.ReleaseManifest) (release.Report, error)
		wantCode   int
		wantOutput string
	}{
		{
			name: "ready", load: func(string) (beta.ReleaseManifest, error) { return beta.ReleaseManifest{}, nil },
			verify: func(ctx context.Context, _ *beta.ReleaseManifest) (release.Report, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) <= 4*time.Minute || time.Until(deadline) > readinessTimeout {
					t.Fatalf("overall deadline = %v, %t", deadline, ok)
				}
				return release.Report{SchemaVersion: release.ReportVersion, Status: "ready"}, nil
			}, wantCode: 0, wantOutput: `"status":"ready"`,
		},
		{
			name: "not ready", load: func(string) (beta.ReleaseManifest, error) { return beta.ReleaseManifest{}, nil },
			verify: func(context.Context, *beta.ReleaseManifest) (release.Report, error) {
				return release.Report{SchemaVersion: release.ReportVersion, Status: "not_ready", FailedCheck: "verification"},
					errors.New("secret database detail")
			}, wantCode: 1, wantOutput: `"failed_check":"verification"`,
		},
		{
			name: "invalid manifest", load: func(string) (beta.ReleaseManifest, error) {
				return beta.ReleaseManifest{}, errors.New("secret parser detail")
			}, wantCode: 2, wantOutput: `"status":"invalid_manifest"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			loadManifest = test.load
			verifyManifest = test.verify
			var stdout, stderr bytes.Buffer
			code := run([]string{"-manifest", "/release.json"}, &stdout, &stderr)
			combined := stdout.String() + stderr.String()
			if code != test.wantCode || !strings.Contains(combined, test.wantOutput) {
				t.Fatalf("exit/output = %d, %q", code, combined)
			}
			if strings.Contains(combined, "secret") {
				t.Fatalf("diagnostic leaked underlying error: %s", combined)
			}
			if test.wantCode == 2 && stdout.Len() != 0 {
				t.Fatalf("invalid manifest wrote stdout: %s", stdout.String())
			}
		})
	}
}
