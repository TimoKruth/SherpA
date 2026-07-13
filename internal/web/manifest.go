package web

import (
	"encoding/json"
	"errors"
)

type manifestView struct {
	Name        string
	Owner       string
	Version     int
	Harness     string
	Summary     string
	ForkedFrom  string
	Tags        []string
	Hooks       []manifestHookView
	MCPServers  []manifestMCPView
	Parameters  []manifestParameterView
	UnknownJSON string
}

type manifestHookView struct {
	Path    string
	Event   string
	Purpose string
}

type manifestMCPView struct {
	Name      string
	Transport string
	Command   string
	Purpose   string
}

type manifestParameterView struct {
	Name        string
	Description string
	Required    bool
}

type manifestWire struct {
	Name       string   `json:"name"`
	Owner      string   `json:"owner"`
	Version    int      `json:"version"`
	Harness    string   `json:"harness"`
	Summary    string   `json:"summary"`
	ForkedFrom string   `json:"forked_from"`
	Tags       []string `json:"tags"`
	Executes   struct {
		Hooks []struct {
			Path    string `json:"path"`
			Event   string `json:"event"`
			Purpose string `json:"purpose"`
		} `json:"hooks"`
		MCPServers []struct {
			Name      string `json:"name"`
			Transport string `json:"transport"`
			Command   string `json:"command"`
			Purpose   string `json:"purpose"`
		} `json:"mcp_servers"`
	} `json:"executes"`
	Parameters []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Required    bool   `json:"required"`
	} `json:"parameters"`
}

func buildManifestView(raw json.RawMessage) (manifestView, error) {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return manifestView{}, errors.New("invalid manifest")
	}
	var wire manifestWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return manifestView{}, errors.New("invalid manifest")
	}
	view := manifestView{
		Name:       wire.Name,
		Owner:      wire.Owner,
		Version:    wire.Version,
		Harness:    wire.Harness,
		Summary:    wire.Summary,
		ForkedFrom: wire.ForkedFrom,
		Tags:       append([]string(nil), wire.Tags...),
		Hooks:      make([]manifestHookView, 0, len(wire.Executes.Hooks)),
		MCPServers: make([]manifestMCPView, 0, len(wire.Executes.MCPServers)),
		Parameters: make([]manifestParameterView, 0, len(wire.Parameters)),
	}
	for _, hook := range wire.Executes.Hooks {
		view.Hooks = append(view.Hooks, manifestHookView{Path: hook.Path, Event: hook.Event, Purpose: hook.Purpose})
	}
	for _, server := range wire.Executes.MCPServers {
		view.MCPServers = append(view.MCPServers, manifestMCPView{
			Name: server.Name, Transport: server.Transport, Command: server.Command, Purpose: server.Purpose,
		})
	}
	for _, parameter := range wire.Parameters {
		view.Parameters = append(view.Parameters, manifestParameterView{
			Name: parameter.Name, Description: parameter.Description, Required: parameter.Required,
		})
	}

	for _, key := range []string{"name", "owner", "version", "harness", "summary", "forked_from", "tags", "executes", "parameters"} {
		delete(fields, key)
	}
	if len(fields) > 0 {
		pretty, err := json.MarshalIndent(fields, "", "  ")
		if err != nil {
			return manifestView{}, errors.New("invalid manifest")
		}
		view.UnknownJSON = string(pretty)
	}
	return view, nil
}
