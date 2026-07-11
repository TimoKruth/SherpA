package harness

import "path/filepath"

type Codex struct{}

func (Codex) Name() string         { return "codex" }
func (Codex) Alias() string        { return "codex" }
func (Codex) ConfigDirEnv() string { return "CODEX_HOME" }
func (Codex) LaunchBin() string    { return "codex" }
func (Codex) LaunchBinEnv() string { return "SHERPA_CODEX_BIN" }
func (Codex) DefaultConfigDir(home string) string {
	return filepath.Join(home, ".codex")
}

// auth.json is Codex's on-disk credential (spike: no keychain). Linked from the
// baseline into each profile; unpublishable.
func (Codex) CredentialFiles() []string     { return []string{"auth.json"} }
func (Codex) SetupStateFilenames() []string { return []string{"auth.json"} }

// No keychain export and no curated onboarding seed (spike: auth.json alone
// authenticates; nothing to strip/curate like Claude's ~/.claude.json).
func (Codex) PrepareBaselineCredentials(baselineDir string) error { return nil }
func (Codex) SetupStateSources(configDir string) []string         { return nil }
func (Codex) CapturedName() string                                { return ".sherpa-codex-setup.json" }
func (Codex) Seed(captured []byte) (string, []byte, error)        { return "", nil, nil }

// LoginSignatures are markers found in auth.json. Non-empty is mandatory for
// the publish barrier.
func (Codex) LoginSignatures() []string {
	return []string{"OPENAI_API_KEY", `"access_token"`, `"refresh_token"`, `"id_token"`, `"tokens"`, `"account_id"`}
}

func (Codex) AllowedPaths() []string {
	return []string{
		"stack.yaml", "README.md", "CHANGELOG.md", "quarantine.json",
		"AGENTS.md", "AGENTS.override.md", "config.toml", "rules/", "skills/", "hooks/",
	}
}

func (Codex) GitignoreContent() string { return codexGitignore }

const codexGitignore = `*
!/.gitignore
!/stack.yaml
!/README.md
!/CHANGELOG.md
!/quarantine.json
!/AGENTS.md
!/AGENTS.override.md
!/config.toml
!/rules/
!/rules/**
!/skills/
!/skills/**
!/hooks/
!/hooks/**
`
