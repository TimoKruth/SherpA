package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestHelpCoversEveryCommand keeps help from drifting: a command added without
// a summary fails here rather than shipping undocumented.
func TestHelpCoversEveryCommand(t *testing.T) {
	for name := range commands {
		if _, ok := commandSummaries[name]; !ok {
			t.Errorf("command %q has no help summary", name)
		}
	}
	for name := range commandSummaries {
		// version is handled directly in Run rather than registered.
		if name == "version" {
			continue
		}
		if _, ok := commands[name]; !ok {
			t.Errorf("help lists %q, which is not a registered command", name)
		}
	}
}

func TestHelpListsCommands(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"help"}, &out, &errb); code != 0 {
		t.Fatalf("sherpa help exited %d: %s", code, errb.String())
	}
	text := out.String()
	for _, want := range []string{"search", "clone", "publish", "remove", "init", "Usage:", "Commands:"} {
		if !strings.Contains(text, want) {
			t.Errorf("help output missing %q:\n%s", want, text)
		}
	}
}

func TestBareInvocationShowsCommands(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run(nil, &out, &errb); code == 0 {
		t.Fatal("bare invocation should exit non-zero")
	}
	// A single usage line left users with no way to discover the commands.
	if !strings.Contains(errb.String(), "search") || !strings.Contains(errb.String(), "Commands:") {
		t.Errorf("bare invocation did not list commands:\n%s", errb.String())
	}
}

func TestUnknownCommandShowsCommands(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"nope"}, &out, &errb); code == 0 {
		t.Fatal("unknown command should exit non-zero")
	}
	text := errb.String()
	if !strings.Contains(text, `unknown command "nope"`) {
		t.Errorf("error should name the command:\n%s", text)
	}
	if !strings.Contains(text, "Commands:") {
		t.Errorf("unknown command should list the available ones:\n%s", text)
	}
}

func TestRegistryURLFallsBackToTheBuildDefault(t *testing.T) {
	t.Setenv("SHERPA_REGISTRY_URL", "")
	if got := registryBaseURL(); got != DefaultRegistryURL {
		t.Fatalf("registryBaseURL() = %q, want the build default %q", got, DefaultRegistryURL)
	}
	if DefaultRegistryURL == "" {
		t.Fatal("a fresh install must have a usable registry without configuration")
	}
}

func TestRegistryURLPrefersTheEnvironment(t *testing.T) {
	t.Setenv("SHERPA_REGISTRY_URL", "https://registry.example.test")
	if got := registryBaseURL(); got != "https://registry.example.test" {
		t.Fatalf("registryBaseURL() = %q, want the configured value", got)
	}
}

func TestRegistryURLIgnoresWhitespaceOnlyConfiguration(t *testing.T) {
	t.Setenv("SHERPA_REGISTRY_URL", "   ")
	if got := registryBaseURL(); got != DefaultRegistryURL {
		t.Fatalf("registryBaseURL() = %q, want the build default", got)
	}
}
