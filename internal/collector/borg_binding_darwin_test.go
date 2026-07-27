//go:build darwin

package collector

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinBorgPathsRevalidateReplacementImmediatelyBeforeStart(t *testing.T) {
	for _, test := range []struct {
		name    string
		restore bool
	}{
		{name: "replacement remains"},
		{name: "replacement restored before check", restore: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testBorgConfig(t)
			cachePath := filepath.Join(config.WorkDir, "cache")
			backupPath := cachePath + ".original"
			sawDerivedPath := false
			system := defaultBorgSystem()
			system.beforeExecStart = func(cmd *exec.Cmd) error {
				sawDerivedPath = borgTestEnvironmentValue(cmd.Env, "BORG_CACHE_DIR") == cachePath
				if err := os.Rename(cachePath, backupPath); err != nil {
					return err
				}
				if err := os.Mkdir(cachePath, 0o700); err != nil {
					return err
				}
				if test.restore {
					if err := os.Remove(cachePath); err != nil {
						return err
					}
					if err := os.Rename(backupPath, cachePath); err != nil {
						return err
					}
				}
				return nil
			}
			backend, logPath := newTestBorgBackendWithSystem(t, config, "exact", system)
			_, err := backend.List(context.Background())
			if !sawDerivedPath {
				t.Fatal("before-Start seam ran before Darwin descriptor paths were derived")
			}
			if test.restore {
				if err != nil {
					t.Fatalf("List: %v", err)
				}
				return
			}
			if err == nil || err.Error() != "collector backend failed" {
				t.Fatalf("List error = %v", err)
			}
			if _, statErr := os.Stat(logPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("replacement state executed Borg: %v", statErr)
			}
		})
	}
}

func TestDarwinBorgPathsRevalidateImmediatelyAfterStart(t *testing.T) {
	for _, test := range []struct {
		name    string
		restore bool
		mode    string
	}{
		{name: "replacement remains", mode: "sleep"},
		{name: "replacement restored before check", restore: true, mode: "exact"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testBorgConfig(t)
			cachePath := filepath.Join(config.WorkDir, "cache")
			backupPath := cachePath + ".original"
			system := defaultBorgSystem()
			system.afterStart = func(*exec.Cmd) error {
				if err := os.Rename(cachePath, backupPath); err != nil {
					return err
				}
				if err := os.Mkdir(cachePath, 0o700); err != nil {
					return err
				}
				if test.restore {
					if err := os.Remove(cachePath); err != nil {
						return err
					}
					if err := os.Rename(backupPath, cachePath); err != nil {
						return err
					}
				}
				return nil
			}
			backend, _ := newTestBorgBackendWithSystem(t, config, test.mode, system)
			_, err := backend.List(context.Background())
			if test.restore {
				if err != nil {
					t.Fatalf("List: %v", err)
				}
				return
			}
			if err == nil || err.Error() != "collector backend failed" {
				t.Fatalf("List error = %v", err)
			}
		})
	}
}

func borgTestEnvironmentValue(environment []string, key string) string {
	prefix := key + "="
	for _, item := range environment {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}
