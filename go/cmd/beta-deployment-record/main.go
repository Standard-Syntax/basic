// Command beta-deployment-record writes the secret-free packaging bill of materials.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Standard-Syntax/basic/go/internal/approval"
	"github.com/Standard-Syntax/basic/go/internal/beta"
	"github.com/Standard-Syntax/basic/go/internal/dockerengine"
	"github.com/Standard-Syntax/basic/go/internal/execution"
	"github.com/Standard-Syntax/basic/go/internal/migration"
	"github.com/Standard-Syntax/basic/go/internal/publication"
	"github.com/Standard-Syntax/basic/go/internal/reasoning/gateway"
	"github.com/Standard-Syntax/basic/go/internal/registry"
	"github.com/Standard-Syntax/basic/go/internal/subprocess"
	"github.com/Standard-Syntax/basic/go/internal/verification"
	"github.com/Standard-Syntax/basic/go/internal/workflow"
)

func main() {
	os.Exit(mainExit())
}

func mainExit() int {
	configPath := flag.String("config", "", "absolute beta deployment path")
	outputPath := flag.String("output", "", "absolute output path")
	flag.Parse()
	deployment, err := beta.LoadDeployment(*configPath)
	if err != nil || !filepath.IsAbs(*outputPath) {
		return fail()
	}
	record, err := buildRecord(context.Background(), &deployment)
	if err != nil {
		return fail()
	}
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fail()
	}
	body = append(body, '\n')
	if err := os.WriteFile(*outputPath, body, 0o600); err != nil {
		return fail()
	}
	return 0
}

func fail() int {
	fmt.Fprintln(os.Stderr, "deployment record generation failed")
	return 1
}

func buildRecord(ctx context.Context, deployment *beta.Deployment) (beta.DeploymentRecord, error) {
	engine, err := dockerengine.NewFromEnvironment()
	if err != nil {
		return beta.DeploymentRecord{}, err
	}
	defer engine.Close()
	for _, image := range []string{deployment.Images.API, deployment.Images.Workflow,
		deployment.Images.Execution, deployment.Images.Verification} {
		actual, err := engine.ImageID(ctx, image)
		if err != nil || actual != image {
			return beta.DeploymentRecord{}, fmt.Errorf("image identity mismatch")
		}
	}
	migrations, err := migration.Describe(workflow.MigrationSource(), registry.MigrationSource(),
		gateway.MigrationSource(), execution.MigrationSource(), verification.MigrationSource(),
		approval.MigrationSource(), publication.MigrationSource())
	if err != nil {
		return beta.DeploymentRecord{}, err
	}
	migrationBody, _ := json.Marshal(migrations)
	configDigest, err := deployment.Digest()
	if err != nil {
		return beta.DeploymentRecord{}, err
	}
	gitVersion, err := command("git", "--version")
	if err != nil {
		return beta.DeploymentRecord{}, err
	}
	manifests, err := beta.DirectoryDigests(deployment.Mounts.Manifests)
	if err != nil {
		return beta.DeploymentRecord{}, err
	}
	prompts, err := beta.DirectoryDigests(deployment.Mounts.Prompts)
	if err != nil {
		return beta.DeploymentRecord{}, err
	}
	return beta.DeploymentRecord{SchemaVersion: beta.DeploymentRecordVersion,
		SourceCommit: deployment.SourceCommit, MigrationDigest: digest(migrationBody),
		ManifestDigests: manifests, PromptDigests: prompts, Images: deployment.Images,
		GitVersion: gitVersion, GoVersion: runtime.Version(), ToolchainVersion: runtime.Version(),
		ConfigurationDigest: configDigest}, nil
}

func command(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = subprocess.Git()
	body, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(body)), err
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
