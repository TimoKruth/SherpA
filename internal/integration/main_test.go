package integration

import (
	"os"
	"testing"
)

// These tests shell out to git. Without an explicit identity they depend on the
// developer's global git configuration, which is why the suite passed locally
// and failed on a clean CI runner with "Committer identity unknown". Pin an
// identity for every git invocation the suite makes so the result is the same
// on a workstation and on a fresh machine.
func TestMain(m *testing.M) {
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME":     "sherpa test",
		"GIT_AUTHOR_EMAIL":    "sherpa-test@example.invalid",
		"GIT_COMMITTER_NAME":  "sherpa test",
		"GIT_COMMITTER_EMAIL": "sherpa-test@example.invalid",
	} {
		if err := os.Setenv(k, v); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}
