package harness

import (
	"fmt"
	"sort"
)

type Harness interface {
	Name() string
	Alias() string
	ConfigDirEnv() string
	DefaultConfigDir(home string) string
	LaunchBin() string
	LaunchBinEnv() string
	CredentialFiles() []string
	PrepareBaselineCredentials(baselineDir string) error
	SetupStateSources(configDir string) []string
	CapturedName() string
	Seed(captured []byte) (targetRel string, content []byte, err error)
	AllowedPaths() []string
	GitignoreContent() string
	SetupStateFilenames() []string
	LoginSignatures() []string
}

var registry = map[string]Harness{"claude-code": ClaudeCode{}, "codex": Codex{}}

func For(name string) (Harness, error) {
	h, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown harness %q (known: %v)", name, Names())
	}
	return h, nil
}

func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func Default() Harness { return ClaudeCode{} }
