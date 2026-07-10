package stack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sherpa/internal/harness"

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
func (m *Manifest) Validate(dir string, h harness.Harness) (violations []string) {
	if h == nil || h.Name() != m.Harness {
		violations = append(violations, fmt.Sprintf("harness %q not supported in phase 1 (claude-code only)", m.Harness))
	}
	declared := map[string]bool{}
	for _, h := range m.Executes.Hooks {
		clean := filepath.Clean(h.Path)
		if h.Path == "" || !filepath.IsLocal(h.Path) || !strings.HasPrefix(filepath.ToSlash(clean), "hooks/") {
			violations = append(violations, "hook path must be a local path under hooks/: "+h.Path)
			continue
		}
		declared[clean] = true
		if _, err := os.Stat(filepath.Join(dir, clean)); err != nil {
			violations = append(violations, "declared hook missing: "+h.Path)
		}
	}
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		switch slashRel := filepath.ToSlash(rel); {
		case strings.HasPrefix(slashRel, "hooks/"):
			// Everything under hooks/ auto-executes, so every file there
			// must be declared, executable bit or not.
			if !declared[filepath.Clean(rel)] {
				violations = append(violations, "undeclared executable: "+rel)
			}
		case strings.HasPrefix(slashRel, "skills/"), strings.HasPrefix(slashRel, "agents/"):
			// Skill/agent helper scripts never auto-execute; they run only
			// when the agent invokes them, mediated by Claude Code's own
			// permission prompts. The manifest's executes section covers the
			// auto-execution surface (hooks + MCP), so these are not flagged.
		default:
			if info.Mode()&0111 != 0 {
				violations = append(violations, "undeclared executable outside hooks/: "+rel)
			}
		}
		return nil
	})
	b, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	switch {
	case err == nil:
		var s map[string]json.RawMessage
		if uerr := json.Unmarshal(b, &s); uerr != nil {
			violations = append(violations, "settings.json unreadable or invalid JSON: "+uerr.Error())
			break
		}
		for _, key := range []string{"hooks", "mcpServers"} {
			if v, ok := s[key]; ok && !emptyJSON(v) {
				violations = append(violations, "settings.json contains live "+key+" (must be quarantined)")
			}
		}
	case !os.IsNotExist(err):
		violations = append(violations, "settings.json unreadable or invalid JSON: "+err.Error())
	}
	return violations
}

// emptyJSON reports whether raw is JSON null, an object with no keys, or an
// array with no elements.
func emptyJSON(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		return len(obj) == 0
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		return len(arr) == 0
	}
	return false
}
