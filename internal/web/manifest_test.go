package web

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildManifestViewKnownAndUnknownFields(t *testing.T) {
	raw := json.RawMessage(`{
      "name":"reviewer","owner":"@alice","version":3,"harness":"codex",
      "summary":"Review code","forked_from":"@origin/base","tags":["review","safe"],
      "executes":{
        "hooks":[{"path":"hooks/check.sh","event":"pre-tool","purpose":"Check writes"}],
        "mcp_servers":[{"name":"docs","transport":"stdio","command":"serve-docs","purpose":"Documentation"}]
      },
      "parameters":[{"name":"strict","description":"Strict mode","required":true}],
      "future":{"text":"<script>unknown</script>"}
    }`)
	view, err := buildManifestView(raw)
	if err != nil {
		t.Fatalf("buildManifestView: %v", err)
	}
	if view.Name != "reviewer" || view.Owner != "@alice" || view.Version != 3 || view.Harness != "codex" || view.Summary != "Review code" || view.ForkedFrom != "@origin/base" {
		t.Fatalf("known fields = %#v", view)
	}
	if strings.Join(view.Tags, ",") != "review,safe" {
		t.Fatalf("tags = %#v", view.Tags)
	}
	if len(view.Hooks) != 1 || view.Hooks[0].Path != "hooks/check.sh" || view.Hooks[0].Event != "pre-tool" || view.Hooks[0].Purpose != "Check writes" {
		t.Fatalf("hooks = %#v", view.Hooks)
	}
	if len(view.MCPServers) != 1 || view.MCPServers[0].Name != "docs" || view.MCPServers[0].Transport != "stdio" || view.MCPServers[0].Command != "serve-docs" {
		t.Fatalf("MCP servers = %#v", view.MCPServers)
	}
	if len(view.Parameters) != 1 || !view.Parameters[0].Required || view.Parameters[0].Name != "strict" {
		t.Fatalf("parameters = %#v", view.Parameters)
	}
	if !strings.Contains(view.UnknownJSON, `"future"`) || !strings.Contains(view.UnknownJSON, `\u003cscript\u003e`) {
		t.Fatalf("unknown JSON = %q", view.UnknownJSON)
	}
}

func TestBuildManifestViewRejectsMalformedOrWrongTypes(t *testing.T) {
	for _, raw := range []json.RawMessage{
		nil,
		json.RawMessage(`null`),
		json.RawMessage(`[]`),
		json.RawMessage(`{"name":`),
		json.RawMessage(`{"tags":"not-an-array"}`),
		json.RawMessage(`{"executes":{"hooks":"not-an-array"}}`),
	} {
		if view, err := buildManifestView(raw); err == nil {
			t.Fatalf("buildManifestView(%s) = %#v, want error", raw, view)
		}
	}
}
