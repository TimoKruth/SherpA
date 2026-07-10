package harness

import (
	"encoding/json"
	"fmt"
	"strings"
)

type ClaudeCode struct{}

func (ClaudeCode) Name() string         { return "claude-code" }
func (ClaudeCode) CapturedName() string { return ".sherpa-setup.json" }

// SetupStateSources: Claude Code keeps setup/identity state in a sibling file
// next to the config dir - configDir "~/.claude" -> "~/.claude.json".
func (ClaudeCode) SetupStateSources(configDir string) []string {
	return []string{configDir + ".json"}
}

// keep is the explicit identity/onboarding/preference whitelist. Everything not
// matched here (and by the family rules below) is dropped - including projects,
// history, and *Cache*. Whitelist fails safe against unknown future keys.
var keep = map[string]bool{
	"hasCompletedOnboarding":               true,
	"hasCompletedClaudeInChromeOnboarding": true,
	"oauthAccount":                         true,
	"userID":                               true,
	"machineID":                            true,
	"installMethod":                        true,
	"firstStartTime":                       true,
	"lastOnboardingVersion":                true,
	"theme":                                true,
	"autoUpdates":                          true,
}

// keepFamily matches setup-safe boolean/timestamp families: migration flags and
// dismissed-callout / seen-notice flags. These carry no secrets or per-project state.
func keepFamily(k string) bool {
	if strings.Contains(k, "Migration") {
		return true
	}
	if strings.HasSuffix(k, "Dismissed") || strings.HasPrefix(k, "hasSeen") || strings.HasPrefix(k, "hasShown") {
		return true
	}
	return false
}

func isPrimitiveJSON(raw json.RawMessage) bool {
	trimmed := strings.TrimLeft(string(raw), " \t\r\n")
	return trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[')
}

func (ClaudeCode) Seed(captured []byte) (string, []byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(captured, &m); err != nil {
		return "", nil, fmt.Errorf("parse setup state: %w", err)
	}
	out := map[string]json.RawMessage{}
	for k, v := range m {
		if keep[k] || (keepFamily(k) && isPrimitiveJSON(v)) {
			out[k] = v
		}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", nil, err
	}
	return ".claude.json", b, nil
}
