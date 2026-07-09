// Package quarantine structurally strips a stack's executable capabilities —
// hooks, MCP servers, and permission grants — out of settings.json and into a
// sidecar quarantine.json, so a cloned stack cannot execute code or auto-grant
// permissions until each capability is explicitly approved (spec §3.5).
package quarantine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	settingsFile   = "settings.json"
	quarantineFile = "quarantine.json"
)

// quar is the on-disk shape of quarantine.json. Hooks are keyed by event and
// stored per matcher entry so each can be approved independently; MCP servers
// are keyed by name; permissions move as a single opaque unit.
type quar struct {
	Hooks       map[string][]json.RawMessage `json:"hooks,omitempty"`
	MCPServers  map[string]json.RawMessage   `json:"mcpServers,omitempty"`
	Permissions json.RawMessage              `json:"permissions,omitempty"`
}

func (q *quar) empty() bool {
	return len(q.Hooks) == 0 && len(q.MCPServers) == 0 && len(q.Permissions) == 0
}

// Strip moves live "hooks", "mcpServers", and "permissions" out of
// dir/settings.json into dir/quarantine.json. It is idempotent: an existing
// quarantine.json is merged into, never clobbered, and a missing or
// capability-free settings.json is a no-op.
func Strip(dir string) error {
	settings, exists, err := loadSettings(dir)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	q, err := loadQuar(dir)
	if err != nil {
		return err
	}

	changed := false
	if v, ok := settings["hooks"]; ok && !emptyJSON(v) {
		var m map[string][]json.RawMessage
		if err := json.Unmarshal(v, &m); err != nil {
			return fmt.Errorf("settings hooks: %w", err)
		}
		if q.Hooks == nil {
			q.Hooks = map[string][]json.RawMessage{}
		}
		for event, entries := range m {
			q.Hooks[event] = append(q.Hooks[event], entries...)
		}
		delete(settings, "hooks")
		changed = true
	}
	if v, ok := settings["mcpServers"]; ok && !emptyJSON(v) {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(v, &m); err != nil {
			return fmt.Errorf("settings mcpServers: %w", err)
		}
		if q.MCPServers == nil {
			q.MCPServers = map[string]json.RawMessage{}
		}
		for name, entry := range m {
			q.MCPServers[name] = entry
		}
		delete(settings, "mcpServers")
		changed = true
	}
	if v, ok := settings["permissions"]; ok && !emptyJSON(v) {
		q.Permissions = v
		delete(settings, "permissions")
		changed = true
	}

	if changed {
		if err := writeJSON(dir, settingsFile, settings); err != nil {
			return err
		}
	}
	if !q.empty() {
		if err := writeJSON(dir, quarantineFile, q); err != nil {
			return err
		}
	}
	return nil
}

// Pending lists quarantined capability IDs: "hook:<event>:<i>" per matcher
// entry, "mcp:<name>", and the single id "permissions". A missing
// quarantine.json yields an empty list.
func Pending(dir string) ([]string, error) {
	q, err := loadQuar(dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, event := range sortedKeys(q.Hooks) {
		for i := range q.Hooks[event] {
			ids = append(ids, fmt.Sprintf("hook:%s:%d", event, i))
		}
	}
	for _, name := range sortedRawKeys(q.MCPServers) {
		ids = append(ids, "mcp:"+name)
	}
	if len(q.Permissions) > 0 && !emptyJSON(q.Permissions) {
		ids = append(ids, "permissions")
	}
	return ids, nil
}

// Approve moves the identified capability back into settings.json and removes
// it from quarantine.json. "all" approves every pending capability at once.
func Approve(dir string, id string) error {
	settings, _, err := loadSettings(dir)
	if err != nil {
		return err
	}
	q, err := loadQuar(dir)
	if err != nil {
		return err
	}

	switch {
	case id == "all":
		for _, event := range sortedKeys(q.Hooks) {
			if err := addHook(settings, event, q.Hooks[event]); err != nil {
				return err
			}
		}
		for _, name := range sortedRawKeys(q.MCPServers) {
			if err := addMCP(settings, name, q.MCPServers[name]); err != nil {
				return err
			}
		}
		if len(q.Permissions) > 0 {
			settings["permissions"] = q.Permissions
		}
		q = &quar{}

	case id == "permissions":
		if len(q.Permissions) == 0 {
			return fmt.Errorf("no pending permissions to approve")
		}
		settings["permissions"] = q.Permissions
		q.Permissions = nil

	case strings.HasPrefix(id, "mcp:"):
		name := strings.TrimPrefix(id, "mcp:")
		entry, ok := q.MCPServers[name]
		if !ok {
			return fmt.Errorf("no pending mcp %q", name)
		}
		if err := addMCP(settings, name, entry); err != nil {
			return err
		}
		delete(q.MCPServers, name)

	case strings.HasPrefix(id, "hook:"):
		event, i, err := parseHookID(id)
		if err != nil {
			return err
		}
		entries := q.Hooks[event]
		if i < 0 || i >= len(entries) {
			return fmt.Errorf("no pending hook %q", id)
		}
		if err := addHook(settings, event, entries[i:i+1]); err != nil {
			return err
		}
		q.Hooks[event] = append(entries[:i:i], entries[i+1:]...)
		if len(q.Hooks[event]) == 0 {
			delete(q.Hooks, event)
		}

	default:
		return fmt.Errorf("unknown capability id %q", id)
	}

	if err := writeJSON(dir, settingsFile, settings); err != nil {
		return err
	}
	return writeJSON(dir, quarantineFile, q)
}

// addHook appends matcher entries to settings["hooks"][event], creating the
// structure as needed.
func addHook(settings map[string]json.RawMessage, event string, entries []json.RawMessage) error {
	m := map[string][]json.RawMessage{}
	if v, ok := settings["hooks"]; ok {
		if err := json.Unmarshal(v, &m); err != nil {
			return fmt.Errorf("live hooks: %w", err)
		}
	}
	m[event] = append(m[event], entries...)
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	settings["hooks"] = b
	return nil
}

// addMCP sets settings["mcpServers"][name], creating the structure as needed.
func addMCP(settings map[string]json.RawMessage, name string, entry json.RawMessage) error {
	m := map[string]json.RawMessage{}
	if v, ok := settings["mcpServers"]; ok {
		if err := json.Unmarshal(v, &m); err != nil {
			return fmt.Errorf("live mcpServers: %w", err)
		}
	}
	m[name] = entry
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	settings["mcpServers"] = b
	return nil
}

func parseHookID(id string) (string, int, error) {
	rest := strings.TrimPrefix(id, "hook:")
	j := strings.LastIndex(rest, ":")
	if j < 0 {
		return "", 0, fmt.Errorf("malformed hook id %q", id)
	}
	i, err := strconv.Atoi(rest[j+1:])
	if err != nil {
		return "", 0, fmt.Errorf("malformed hook id %q", id)
	}
	return rest[:j], i, nil
}

func loadSettings(dir string) (map[string]json.RawMessage, bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, settingsFile))
	if os.IsNotExist(err) {
		return map[string]json.RawMessage{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, false, fmt.Errorf("settings.json: %w", err)
	}
	return m, true, nil
}

func loadQuar(dir string) (*quar, error) {
	q := &quar{}
	b, err := os.ReadFile(filepath.Join(dir, quarantineFile))
	if os.IsNotExist(err) {
		return q, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, q); err != nil {
		return nil, fmt.Errorf("quarantine.json: %w", err)
	}
	return q, nil
}

func writeJSON(dir, name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sortedKeys(m map[string][]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedRawKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// emptyJSON reports whether raw is JSON null, an object with no keys, or an
// array with no elements. Local to this package by design (spec: do not import
// internal/stack).
func emptyJSON(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return true
	}
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
