package beta

// DeploymentRecord is the immutable, secret-free packaging bill of materials.
type DeploymentRecord struct {
	SchemaVersion       string            `json:"schema_version"`
	SourceCommit        string            `json:"source_commit"`
	MigrationDigest     string            `json:"migration_digest"`
	ManifestDigests     map[string]string `json:"manifest_digests"`
	PromptDigests       map[string]string `json:"prompt_digests"`
	Images              DeploymentImages  `json:"images"`
	GitVersion          string            `json:"git_version"`
	GoVersion           string            `json:"go_version"`
	ToolchainVersion    string            `json:"toolchain_version"`
	ConfigurationDigest string            `json:"configuration_digest"`
}

const DeploymentRecordVersion = "beta_deployment_record.v1"
