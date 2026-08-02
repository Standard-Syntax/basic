// Package subprocess defines closed child-process environments.
package subprocess

import (
	"errors"
	"path/filepath"
	"regexp"
)

var sshKeyPattern = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)

func Git(extra ...string) []string {
	base := []string{
		"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_OPTIONAL_LOCKS=0",
	}
	return append(base, extra...)
}

func RemoteGit(sshKey string, extra ...string) ([]string, error) {
	environment := Git(
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=",
		"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=core.hooksPath",
		"GIT_CONFIG_VALUE_0=/dev/null", "GIT_CONFIG_KEY_1=credential.interactive",
		"GIT_CONFIG_VALUE_1=never",
	)
	if sshKey != "" {
		if !filepath.IsAbs(sshKey) || filepath.Clean(sshKey) != sshKey ||
			!sshKeyPattern.MatchString(sshKey) {
			return nil, errors.New("invalid Git SSH credential path")
		}
		environment = append(environment,
			"GIT_SSH_COMMAND=ssh -i '"+sshKey+"' -o IdentitiesOnly=yes -o BatchMode=yes",
		)
	}
	return append(environment, extra...), nil
}

func Docker(host, apiVersion string) []string {
	environment := []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	if host != "" {
		environment = append(environment, "DOCKER_HOST="+host)
	}
	if apiVersion != "" {
		environment = append(environment, "DOCKER_API_VERSION="+apiVersion)
	}
	return environment
}
