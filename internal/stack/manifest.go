package stack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type HookDecl struct {
	Path, Event, Purpose string
}
type MCPDecl struct {
	Name, Transport, Command, Purpose string
}
type Parameter struct {
	Name, Description string
	Required          bool
}
type Executes struct {
	Hooks      []HookDecl `yaml:"hooks"`
	MCPServers []MCPDecl  `yaml:"mcp_servers"`
}
type Manifest struct {
	Name       string      `yaml:"name"`
	Owner      string      `yaml:"owner"`
	Version    int         `yaml:"version"`
	Harness    string      `yaml:"harness"`
	Summary    string      `yaml:"summary"`
	ForkedFrom string      `yaml:"forked_from"`
	Tags       []string    `yaml:"tags"`
	Executes   Executes    `yaml:"executes"`
	Parameters []Parameter `yaml:"parameters"`
}

func Parse(b []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Name == "" || m.Version < 1 {
		return nil, fmt.Errorf("manifest needs name and version >= 1")
	}
	return &m, nil
}

// Validate checks the manifest against the stack directory contents.
func (m *Manifest) Validate(dir string) (violations []string) {
	if m.Harness != "claude-code" {
		violations = append(violations, fmt.Sprintf("harness %q not supported in phase 1 (claude-code only)", m.Harness))
	}
	declared := map[string]bool{}
	for _, h := range m.Executes.Hooks {
		declared[filepath.Clean(h.Path)] = true
		if _, err := os.Stat(filepath.Join(dir, h.Path)); err != nil {
			violations = append(violations, "declared hook missing: "+h.Path)
		}
	}
	filepath.Walk(filepath.Join(dir, "hooks"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if !declared[filepath.Clean(rel)] {
			violations = append(violations, "undeclared executable: "+rel)
		}
		return nil
	})
	if b, err := os.ReadFile(filepath.Join(dir, "settings.json")); err == nil {
		var s map[string]json.RawMessage
		if json.Unmarshal(b, &s) == nil {
			for _, key := range []string{"hooks", "mcpServers"} {
				if v, ok := s[key]; ok && string(v) != "{}" && string(v) != "[]" && string(v) != "null" {
					violations = append(violations, "settings.json contains live "+key+" (must be quarantined)")
				}
			}
		}
	}
	return violations
}
