// Command beta-compose-env renders a validated deployment as a Docker Compose
// environment file. Values are paths and immutable IDs only; no secret bytes
// are read or emitted.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Standard-Syntax/basic/go/internal/beta"
)

func main() {
	path := flag.String("config", "", "absolute beta_deployment.v1 path")
	flag.Parse()
	value, err := beta.LoadDeployment(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid beta deployment")
		os.Exit(2)
	}
	values := map[string]string{
		"BETA_API_IMAGE": value.Images.API, "BETA_WORKFLOW_IMAGE": value.Images.Workflow,
		"BETA_EXECUTION_IMAGE": value.Images.Execution, "BETA_VERIFICATION_IMAGE": value.Images.Verification,
		"BETA_API_CONFIG": value.Services.APIConfig, "BETA_WORKFLOW_CONFIG": value.Services.WorkflowConfig,
		"BETA_REPOSITORY_ROOT": value.Mounts.Repository, "BETA_CAS_ROOT": value.Mounts.CAS,
		"BETA_WORKTREE_ROOT": value.Mounts.Worktrees, "BETA_VERIFICATION_ROOT": value.Mounts.Verification,
		"BETA_MANIFEST_ROOT": value.Mounts.Manifests, "BETA_PROMPT_ROOT": value.Mounts.Prompts,
		"BETA_CONFIGURATION_ROOT":       value.Mounts.Configuration,
		"BETA_MINIMAX_CREDENTIAL_FILE":  value.Credentials.MiniMax,
		"BETA_MINIMAX_CREDENTIAL_ROOT":  filepath.Dir(value.Credentials.MiniMax),
		"BETA_POSTGRES_PASSWORD_FILE":   value.Credentials.DatabasePassword,
		"BETA_POSTGRES_CREDENTIAL_ROOT": filepath.Dir(value.Credentials.DatabasePassword),
		"BETA_GITHUB_CREDENTIAL_FILE":   value.Credentials.GitHub,
		"BETA_GIT_PUSH_CREDENTIAL_FILE": value.Credentials.GitPush,
		"BETA_DOCKER_GID":               fmt.Sprint(value.DockerSocketGroup),
	}
	for key, item := range values {
		if strings.ContainsAny(item, "\r\n") {
			fmt.Fprintln(os.Stderr, "unsafe deployment value")
			os.Exit(2)
		}
		fmt.Printf("%s=%s\n", key, item)
	}
}
