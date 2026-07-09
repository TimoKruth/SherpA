package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{"nope"}, &out, &errb)
	if code == 0 {
		t.Fatal("want nonzero exit for unknown command")
	}
	if !strings.Contains(errb.String(), "unknown command") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestRunVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"version"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "sherpa") {
		t.Fatalf("stdout = %q", out.String())
	}
}
