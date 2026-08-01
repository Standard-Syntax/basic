// Command beta-readiness verifies a release manifest against immutable release evidence.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Standard-Syntax/basic/go/internal/beta"
	"github.com/Standard-Syntax/basic/go/internal/release"
)

func main() { os.Exit(mainExit()) }

const readinessTimeout = 5 * time.Minute

var (
	loadManifest   = beta.LoadReleaseManifest
	verifyManifest = func(ctx context.Context, manifest *beta.ReleaseManifest) (release.Report, error) {
		return release.NewVerifier().Verify(ctx, manifest)
	}
)

func mainExit() int { return run(os.Args[1:], os.Stdout, os.Stderr) }

func run(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("beta-readiness", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	manifestPath := flags.String("manifest", "", "absolute beta_release_manifest.v1 path")
	if err := flags.Parse(arguments); err != nil {
		fmt.Fprintln(stderr, `{"schema_version":"beta_readiness_report.v1","status":"invalid_manifest"}`)
		return 2
	}
	manifest, err := loadManifest(*manifestPath)
	if err != nil {
		fmt.Fprintln(stderr, `{"schema_version":"beta_readiness_report.v1","status":"invalid_manifest"}`)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), readinessTimeout)
	defer cancel()
	report, err := verifyManifest(ctx, &manifest)
	if err != nil {
		body, _ := json.Marshal(report)
		fmt.Fprintln(stdout, string(body))
		return 1
	}
	body, err := json.Marshal(report)
	if err != nil {
		return 1
	}
	fmt.Fprintln(stdout, string(body))
	return 0
}
