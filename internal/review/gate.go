package review

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"sherpa/internal/quarantine"
	"sherpa/internal/stack"
)

type Mode int

const (
	Interactive Mode = iota
	ApproveAll
	KeepQuarantined
)

type quarantineFile struct {
	Permissions json.RawMessage `json:"permissions,omitempty"`
}

// RunGate shows every pending capability with its declared purpose and, for
// hooks, the full script content. mode: Interactive | ApproveAll |
// KeepQuarantined. Returns the ids the user approved. Reads y/n/a(ll)/q from in.
func RunGate(dir string, m *stack.Manifest, mode Mode, in io.Reader, out io.Writer) ([]string, error) {
	pending, err := quarantine.Pending(dir)
	if err != nil {
		return nil, err
	}

	switch mode {
	case KeepQuarantined:
		if err := renderCapabilities(out, dir, m, pending); err != nil {
			return nil, err
		}
		return nil, nil
	case ApproveAll:
		if err := renderCapabilities(out, dir, m, pending); err != nil {
			return nil, err
		}
		if len(pending) == 0 {
			return nil, nil
		}
		if err := quarantine.Approve(dir, "all"); err != nil {
			return nil, err
		}
		return pending, nil
	case Interactive:
	default:
		return nil, fmt.Errorf("unknown review mode %d", mode)
	}

	scanner := bufio.NewScanner(in)
	approved := []string{}
	approvedSet := map[string]bool{}
	for i, id := range pending {
		if err := renderCapability(out, dir, m, id); err != nil {
			return approved, err
		}
		for {
			fmt.Fprint(out, "approve? [y/n/a/q] ")
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					return approved, err
				}
				return approved, nil
			}
			answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
			switch answer {
			case "y", "yes":
				if err := quarantine.Approve(dir, id); err != nil {
					return approved, err
				}
				approved = append(approved, id)
				approvedSet[id] = true
				goto next
			case "n", "no", "":
				goto next
			case "a", "all":
				if err := quarantine.Approve(dir, "all"); err != nil {
					return approved, err
				}
				for _, rest := range pending[i:] {
					if !approvedSet[rest] {
						approved = append(approved, rest)
						approvedSet[rest] = true
					}
				}
				return approved, nil
			case "q", "quit":
				return approved, nil
			default:
				fmt.Fprintln(out, "please answer y, n, a, or q")
			}
		}
	next:
	}
	return approved, nil
}

func renderCapabilities(out io.Writer, dir string, m *stack.Manifest, ids []string) error {
	for _, id := range ids {
		if err := renderCapability(out, dir, m, id); err != nil {
			return err
		}
	}
	return nil
}

func renderCapability(out io.Writer, dir string, m *stack.Manifest, id string) error {
	fmt.Fprintf(out, "\n%s\n", id)
	switch {
	case strings.HasPrefix(id, "hook:"):
		event, err := hookEvent(id)
		if err != nil {
			return err
		}
		hooks := hooksForEvent(m, event)
		fmt.Fprintf(out, "type: hook\n")
		fmt.Fprintf(out, "event: %s\n", event)
		if len(hooks) == 0 {
			fmt.Fprintln(out, "purpose: (not declared)")
			return nil
		}
		for _, h := range hooks {
			fmt.Fprintf(out, "path: %s\n", h.Path)
			fmt.Fprintf(out, "purpose: %s\n", h.Purpose)
			if err := printHookScript(out, dir, h.Path); err != nil {
				return err
			}
		}
	case strings.HasPrefix(id, "mcp:"):
		name := strings.TrimPrefix(id, "mcp:")
		fmt.Fprintln(out, "type: mcp server")
		fmt.Fprintf(out, "name: %s\n", name)
		if d, ok := mcpByName(m, name); ok {
			fmt.Fprintf(out, "purpose: %s\n", d.Purpose)
			if d.Command != "" {
				fmt.Fprintf(out, "command: %s\n", d.Command)
			}
		} else {
			fmt.Fprintln(out, "purpose: (not declared)")
		}
	case id == "permissions":
		fmt.Fprintln(out, "type: permissions")
		fmt.Fprintln(out, "purpose: pre-approves tool permissions inside Claude Code sessions, weakening Claude Code permission prompts")
		raw, err := permissionsRaw(dir)
		if err != nil {
			return err
		}
		if len(raw) > 0 {
			fmt.Fprintln(out, "value:")
			printIndented(out, string(raw))
		}
	default:
		fmt.Fprintln(out, "type: unknown")
	}
	return nil
}

func hookEvent(id string) (string, error) {
	rest := strings.TrimPrefix(id, "hook:")
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		return "", fmt.Errorf("malformed hook id %q", id)
	}
	return rest[:i], nil
}

func hooksForEvent(m *stack.Manifest, event string) []stack.HookDecl {
	if m == nil {
		return nil
	}
	var out []stack.HookDecl
	for _, h := range m.Executes.Hooks {
		if h.Event == event {
			out = append(out, h)
		}
	}
	return out
}

func mcpByName(m *stack.Manifest, name string) (stack.MCPDecl, bool) {
	if m == nil {
		return stack.MCPDecl{}, false
	}
	for _, s := range m.Executes.MCPServers {
		if s.Name == name {
			return s, true
		}
	}
	return stack.MCPDecl{}, false
}

func printHookScript(out io.Writer, dir, rel string) error {
	if rel == "" || !filepath.IsLocal(rel) {
		return fmt.Errorf("invalid hook path %q", rel)
	}
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		return fmt.Errorf("read hook %s: %w", rel, err)
	}
	fmt.Fprintln(out, "script:")
	printIndented(out, string(b))
	return nil
}

func permissionsRaw(dir string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, "quarantine.json"))
	if err != nil {
		return nil, err
	}
	var q quarantineFile
	if err := json.Unmarshal(b, &q); err != nil {
		return nil, fmt.Errorf("quarantine.json: %w", err)
	}
	if len(q.Permissions) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(q.Permissions, &v); err != nil {
		return q.Permissions, nil
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return q.Permissions, nil
	}
	return pretty, nil
}

func printIndented(out io.Writer, s string) {
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		fmt.Fprintf(out, "  %s\n", line)
	}
}
