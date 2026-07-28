package collector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestBorgCreateReconcilesAmbiguousOutcomes(t *testing.T) {
	tests := []struct {
		name      string
		mode      string
		configure func(*BorgBackend)
		wantError string
		wantCalls int
	}{
		{name: "nonzero exact present", mode: "create-failure-exact", wantCalls: 2},
		{name: "timeout exact present", mode: "create-timeout-exact", configure: func(backend *BorgBackend) {
			backend.config.CreateTimeout = borgTestTimeout
		}, wantCalls: 2},
		{name: "output ambiguity exact present", mode: "create-overflow-exact", wantCalls: 2},
		{name: "wait-delay ambiguity exact present", mode: "create-waitdelay-exact", wantCalls: 2},
		{name: "nonzero absent preserves class", mode: "create-failure-absent", wantError: "collector backend failed", wantCalls: 2},
		{name: "verification unavailable", mode: "create-failure-list-failure", wantError: "collector remote verification failed", wantCalls: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend, logPath := newTestBorgBackend(t, test.mode)
			if test.configure != nil {
				test.configure(backend)
			}
			err := backend.Create(context.Background(), testPendingObject(t))
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
			} else if err == nil || err.Error() != test.wantError {
				t.Fatalf("Create error = %v, want %q", err, test.wantError)
			}
			if calls := readBorgInvocations(t, logPath); len(calls) != test.wantCalls {
				t.Fatalf("invocations = %d, want %d: %#v", len(calls), test.wantCalls, calls)
			}
		})
	}
}

func TestBorgCreateCancellationDoesNotStartReconciliation(t *testing.T) {
	backend, logPath := newTestBorgBackend(t, "create-timeout-exact")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- backend.Create(ctx, testPendingObject(t)) }()
	waitForInvocationCount(t, logPath, 1)
	cancel()
	select {
	case err := <-done:
		if err == nil || err.Error() != "collector backend cancelled" {
			t.Fatalf("Create error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled create did not return")
	}
	if calls := readBorgInvocations(t, logPath); len(calls) != 1 {
		t.Fatalf("cancelled create triggered reconciliation: %#v", calls)
	}
}

func TestBorgCreateCancellationDuringReconciliationReturnsCancelled(t *testing.T) {
	backend, logPath := newTestBorgBackend(t, "create-success-list-sleep")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- backend.Create(ctx, testPendingObject(t)) }()
	waitForInvocationCount(t, logPath, 2)
	cancel()
	select {
	case err := <-done:
		if err == nil || err.Error() != "collector backend cancelled" {
			t.Fatalf("Create error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled reconciliation did not return")
	}
}

func TestBorgCreateRejectsUnsafeEncryptedInputBeforeExecution(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(testing.TB, *PendingObject)
	}{
		{name: "symlink", mutate: func(t testing.TB, object *PendingObject) {
			target := filepath.Join(newPrivateDir(t), "target")
			if err := os.WriteFile(target, []byte("encrypted-object-canary"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(object.EncryptedPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, object.EncryptedPath); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "directory", mutate: func(t testing.TB, object *PendingObject) {
			if err := os.Remove(object.EncryptedPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(object.EncryptedPath, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrong mode", mutate: func(t testing.TB, object *PendingObject) {
			if err := os.Chmod(object.EncryptedPath, 0o640); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "size mismatch", mutate: func(_ testing.TB, object *PendingObject) {
			object.EncryptedSize++
		}},
		{name: "parent wrong mode", mutate: func(t testing.TB, object *PendingObject) {
			if err := os.Chmod(filepath.Dir(object.EncryptedPath), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink parent", mutate: func(t testing.TB, object *PendingObject) {
			realParent := filepath.Dir(object.EncryptedPath)
			linkParent := filepath.Join(t.TempDir(), "object-parent-link")
			if err := os.Symlink(realParent, linkParent); err != nil {
				t.Fatal(err)
			}
			object.EncryptedPath = filepath.Join(linkParent, filepath.Base(object.EncryptedPath))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := testPendingObject(t)
			test.mutate(t, &object)
			backend, logPath := newTestBorgBackend(t, "exact")
			err := backend.Create(context.Background(), object)
			if err == nil || err.Error() != "collector backend failed" {
				t.Fatalf("Create error = %v", err)
			}
			if _, statErr := os.Stat(logPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("unsafe input executed Borg: %v", statErr)
			}
		})
	}
}

func TestBorgCreateRejectsWrongOwnerInputWhereSupported(t *testing.T) {
	object := testPendingObject(t)
	if err := os.Chown(object.EncryptedPath, os.Geteuid()+1, os.Getegid()); err != nil {
		t.Skipf("cannot change test input owner: %v", err)
	}
	backend, logPath := newTestBorgBackend(t, "exact")
	err := backend.Create(context.Background(), object)
	if err == nil || err.Error() != "collector backend failed" {
		t.Fatalf("Create error = %v", err)
	}
	if _, statErr := os.Stat(logPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("wrong-owner input executed Borg: %v", statErr)
	}
}

func TestBorgCreateRejectsReplacementDuringCreateEvenWhenArchiveExists(t *testing.T) {
	object := testPendingObject(t)
	backend, logPath := newTestBorgBackendForObject(t, "replace-during-create-exact", object.EncryptedPath)
	err := backend.Create(context.Background(), object)
	if err == nil || err.Error() != "collector backend failed" {
		t.Fatalf("Create error = %v", err)
	}
	if calls := readBorgInvocations(t, logPath); len(calls) != 1 {
		t.Fatalf("replaced input continued to remote verification: %#v", calls)
	}
}

func TestBorgCreateRechecksInputBeforeAcceptingReconciledSuccess(t *testing.T) {
	object := testPendingObject(t)
	backend, _ := newTestBorgBackendForObject(t, "replace-during-list-exact", object.EncryptedPath)
	err := backend.Create(context.Background(), object)
	if err == nil || err.Error() != "collector backend failed" {
		t.Fatalf("Create error = %v", err)
	}
}

func TestBorgRunnerAlwaysTerminatesDedicatedProcessGroup(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process-group assertion targets supported runtime platforms")
	}
	tests := []struct {
		name      string
		mode      string
		wantError string
	}{
		{name: "descendant holds pipe", mode: "tree-holds-pipe", wantError: "collector backend failed"},
		{name: "descendant closes stdio", mode: "tree-closed-stdio", wantError: "collector backend failed"},
		{name: "successful direct child", mode: "tree-success"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pidPath := filepath.Join(t.TempDir(), "child-pid")
			t.Setenv("COLLECTOR_BORG_CHILD_PID", pidPath)
			backend, _ := newTestBorgBackend(t, test.mode)
			backend.config.QueryTimeout = 5 * time.Second
			_, err := backend.List(context.Background())
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("List: %v", err)
				}
			} else if err == nil || err.Error() != test.wantError {
				t.Fatalf("List error = %v, want %q", err, test.wantError)
			}
			assertProcessExited(t, waitForChildPID(t, pidPath))
		})
	}
}

func TestBorgListParsesZoneLessStartInBackendLocalTime(t *testing.T) {
	location, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	globalLocal := time.Local
	logPath := filepath.Join(t.TempDir(), "borg-invocations.jsonl")
	t.Setenv("COLLECTOR_BORG_HELPER", "1")
	t.Setenv("COLLECTOR_BORG_MODE", "local-list")
	t.Setenv("COLLECTOR_BORG_LOG", logPath)
	backend, err := newBorgBackend(testBorgConfig(t), location)
	if err != nil {
		t.Fatalf("newBorgBackend: %v", err)
	}
	archives, err := backend.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	assertArchiveNames(t, archives, []string{"sherpa-local-noon", "sherpa-zoned-one"})
	if !archives[0].StartedAt.Equal(time.Date(2024, 1, 15, 20, 0, 0, 0, time.UTC)) || archives[0].StartedAt.Location() != location {
		t.Fatalf("zone-less start = %v in %v", archives[0].StartedAt, archives[0].StartedAt.Location())
	}
	_, offset := archives[1].StartedAt.Zone()
	if offset != -8*60*60 {
		t.Fatalf("RFC3339 offset = %d", offset)
	}
	if time.Local != globalLocal {
		t.Fatal("backend timestamp parsing mutated global time.Local")
	}
}

func TestNewBorgBackendRejectsRelativeAndUncleanSSHPathsWithoutDisclosure(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*BorgConfig, string)
	}{
		{name: "relative SSH key", mutate: func(config *BorgConfig, canary string) { config.SSHKeyFile = canary }},
		{name: "unclean SSH key", mutate: func(config *BorgConfig, canary string) {
			config.SSHKeyFile = t.TempDir() + string(os.PathSeparator) + "segment" + string(os.PathSeparator) + ".." + string(os.PathSeparator) + canary
		}},
		{name: "relative known hosts", mutate: func(config *BorgConfig, canary string) { config.KnownHostsFile = canary }},
		{name: "unclean known hosts", mutate: func(config *BorgConfig, canary string) {
			config.KnownHostsFile = t.TempDir() + string(os.PathSeparator) + "segment" + string(os.PathSeparator) + ".." + string(os.PathSeparator) + canary
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			canary := "private-path-canary-" + strings.ReplaceAll(test.name, " ", "-")
			config := testBorgConfig(t)
			test.mutate(&config, canary)
			_, err := NewBorgBackend(config)
			if err == nil || err.Error() != "collector backend unavailable" {
				t.Fatalf("NewBorgBackend error = %v", err)
			}
			if strings.Contains(err.Error(), canary) {
				t.Fatalf("configuration error exposed %q: %v", canary, err)
			}
		})
	}
}

func TestNewBorgBackendCreatesOnlyPrivateDescriptorRelativeState(t *testing.T) {
	config := testBorgConfig(t)
	backend, err := NewBorgBackend(config)
	if err != nil {
		t.Fatalf("NewBorgBackend: %v", err)
	}
	wantEnv := map[string]string{
		"BORG_CACHE_DIR":    borgFDPath(3),
		"BORG_CONFIG_DIR":   borgFDPath(4),
		"BORG_SECURITY_DIR": borgFDPath(5),
	}
	gotEnv := make(map[string]string)
	for _, item := range backend.environment {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			gotEnv[key] = value
		}
	}
	for key, path := range wantEnv {
		if gotEnv[key] != path {
			t.Fatalf("%s = %q, want %q", key, gotEnv[key], path)
		}
	}
	for _, name := range []string{"cache", "config", "security"} {
		path := filepath.Join(config.WorkDir, name)
		var stat unix.Stat_t
		if err := unix.Lstat(path, &stat); err != nil {
			t.Fatal(err)
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o7777 != 0o700 || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) {
			t.Fatalf("unsafe Borg state metadata for %s: mode=%#o uid=%d gid=%d", path, stat.Mode, stat.Uid, stat.Gid)
		}
	}
}

func TestNewBorgBackendRejectsUnsafeWorkStateWithoutMutation(t *testing.T) {
	tests := []struct {
		name  string
		setup func(testing.TB, string) string
	}{
		{name: "missing root", setup: func(t testing.TB, _ string) string { return filepath.Join(t.TempDir(), "missing-root") }},
		{name: "symlink root", setup: func(t testing.TB, _ string) string {
			target := newPrivateDir(t)
			link := filepath.Join(t.TempDir(), "work-link")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
		{name: "wrong mode root", setup: func(t testing.TB, root string) string {
			if err := os.Chmod(root, 0o755); err != nil {
				t.Fatal(err)
			}
			return root
		}},
		{name: "symlink child", setup: func(t testing.TB, root string) string {
			target := newPrivateDir(t)
			if err := os.Symlink(target, filepath.Join(root, "cache")); err != nil {
				t.Fatal(err)
			}
			return root
		}},
		{name: "wrong mode child", setup: func(t testing.TB, root string) string {
			child := filepath.Join(root, "cache")
			if err := os.Mkdir(child, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(child, 0o755); err != nil {
				t.Fatal(err)
			}
			return root
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testBorgConfig(t)
			config.WorkDir = test.setup(t, config.WorkDir)
			_, err := NewBorgBackend(config)
			if err == nil || err.Error() != "collector backend unavailable" {
				t.Fatalf("NewBorgBackend error = %v", err)
			}
			if test.name == "wrong mode root" || test.name == "wrong mode child" {
				path := config.WorkDir
				if test.name == "wrong mode child" {
					path = filepath.Join(config.WorkDir, "cache")
				}
				info, statErr := os.Stat(path)
				if statErr != nil {
					t.Fatal(statErr)
				}
				if info.Mode().Perm() != 0o755 {
					t.Fatalf("unsafe child mode was changed to %04o", info.Mode().Perm())
				}
			}
		})
	}
}

func TestNewBorgBackendRejectsWrongOwnerWorkStateWhereSupported(t *testing.T) {
	tests := []struct {
		name string
		path func(testing.TB, BorgConfig) string
	}{
		{name: "root", path: func(_ testing.TB, config BorgConfig) string { return config.WorkDir }},
		{name: "child", path: func(t testing.TB, config BorgConfig) string {
			child := filepath.Join(config.WorkDir, "cache")
			if err := os.Mkdir(child, 0o700); err != nil {
				t.Fatal(err)
			}
			return child
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testBorgConfig(t)
			path := test.path(t, config)
			if err := os.Chown(path, os.Geteuid()+1, os.Getegid()); err != nil {
				t.Skipf("cannot change test directory owner: %v", err)
			}
			_, err := NewBorgBackend(config)
			if err == nil || err.Error() != "collector backend unavailable" {
				t.Fatalf("NewBorgBackend error = %v", err)
			}
		})
	}
}

func TestNewBorgBackendWorkErrorsDoNotExposePrivatePaths(t *testing.T) {
	config := testBorgConfig(t)
	privateCanary := filepath.Join(config.WorkDir, "private-state-canary")
	if err := os.Symlink(newPrivateDir(t), privateCanary); err != nil {
		t.Fatal(err)
	}
	config.WorkDir = privateCanary
	_, err := NewBorgBackend(config)
	if err == nil || err.Error() != "collector backend unavailable" {
		t.Fatalf("NewBorgBackend error = %v", err)
	}
	if strings.Contains(err.Error(), privateCanary) || strings.Contains(err.Error(), config.WorkDir) {
		t.Fatalf("configuration error exposed private path: %v", err)
	}
}

func TestBorgStateEnvironmentContainsOnlyExpectedPaths(t *testing.T) {
	backend, _ := newTestBorgBackend(t, "exact")
	var got []string
	for _, item := range backend.environment {
		if strings.HasPrefix(item, "BORG_") {
			got = append(got, item)
		}
	}
	if len(got) != 6 {
		t.Fatalf("Borg environment entries = %#v", got)
	}
	for _, path := range []string{backend.config.SSHKeyFile, backend.config.KnownHostsFile, backend.config.WorkDir} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			t.Fatalf("unsafe configured path = %q", path)
		}
	}
}

func waitForInvocationCount(t testing.TB, path string, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(invocationLines(data)) >= count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("did not observe %d Borg invocations", count)
}

func assertProcessExited(t testing.TB, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatalf("descendant process %d survived Borg runner cleanup", pid)
	}
}

func assertArchiveNames(t testing.TB, archives []ArchiveInfo, want []string) {
	t.Helper()
	got := make([]string, len(archives))
	for i := range archives {
		got[i] = archives[i].Name
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("archive names = %v, want %v", got, want)
	}
}
