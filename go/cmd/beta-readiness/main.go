// Command beta-readiness verifies a release manifest against immutable release evidence.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/Standard-Syntax/basic/go/internal/beta"
	"github.com/Standard-Syntax/basic/go/internal/release"
)

func main() { os.Exit(mainExit()) }

func mainExit() int {
	manifestPath := flag.String("manifest", "", "absolute beta_release_manifest.v1 path")
	flag.Parse()
	manifest, err := beta.LoadReleaseManifest(*manifestPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, `{"schema_version":"beta_readiness_report.v1","status":"invalid_manifest"}`)
		return 2
	}
	report, err := release.NewVerifier().Verify(context.Background(), manifest)
	if err != nil {
		body, _ := json.Marshal(report)
		fmt.Fprintln(os.Stdout, string(body))
		return 1
	}
	body, err := json.Marshal(report)
	if err != nil {
		return 1
	}
	fmt.Fprintln(os.Stdout, string(body))
	return 0
}
