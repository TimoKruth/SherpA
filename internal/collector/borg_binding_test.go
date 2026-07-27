package collector

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestBorgCreateReadsDescriptorPinnedStagingEntryAcrossSourceABA(t *testing.T) {
	tests := []struct {
		name string
		swap func(testing.TB, PendingObject) func()
	}{
		{
			name: "source entry",
			swap: func(t testing.TB, object PendingObject) func() {
				backup := object.EncryptedPath + ".original"
				if err := os.Rename(object.EncryptedPath, backup); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(object.EncryptedPath, []byte("replacement-object-data"), 0o600); err != nil {
					t.Fatal(err)
				}
				return func() {
					if err := os.Remove(object.EncryptedPath); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(backup, object.EncryptedPath); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "source parent",
			swap: func(t testing.TB, object PendingObject) func() {
				parent := filepath.Dir(object.EncryptedPath)
				backup := parent + ".original"
				if err := os.Rename(parent, backup); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(parent, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(parent, filepath.Base(object.EncryptedPath)), []byte("replacement-object-data"), 0o600); err != nil {
					t.Fatal(err)
				}
				return func() {
					if err := os.RemoveAll(parent); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(backup, parent); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := testPendingObject(t)
			config := testBorgConfig(t)
			capturePath := filepath.Join(t.TempDir(), "captured-input")
			readReady := filepath.Join(t.TempDir(), "read-ready")
			helperRelease := filepath.Join(t.TempDir(), "helper-release")
			t.Setenv("COLLECTOR_BORG_CAPTURE", capturePath)
			t.Setenv("COLLECTOR_BORG_READ_READY", readReady)
			t.Setenv("COLLECTOR_BORG_HELPER_RELEASE", helperRelease)
			bound := make(chan struct{})
			start := make(chan struct{})
			system := defaultBorgSystem()
			system.beforeStart = func(cmd *exec.Cmd) error {
				if len(cmd.Args) > 1 && cmd.Args[1] == "create" {
					close(bound)
					<-start
				}
				return nil
			}
			backend, _ := newTestBorgBackendWithSystem(t, config, "capture-create-exact", system)
			done := make(chan error, 1)
			go func() { done <- backend.Create(context.Background(), object) }()
			<-bound
			restore := test.swap(t, object)
			close(start)
			waitForFile(t, readReady)
			restore()
			if err := os.WriteFile(helperRelease, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatalf("Create: %v", err)
			}
			captured, err := os.ReadFile(capturePath)
			if err != nil {
				t.Fatal(err)
			}
			if string(captured) != "encrypted-object-canary" {
				t.Fatalf("Borg read %q", captured)
			}
			assertNoBorgStages(t, config.WorkDir)
		})
	}
}

func TestBorgCreateMaterializesHardLinkFromHeldDescriptor(t *testing.T) {
	object := testPendingObject(t)
	config := testBorgConfig(t)
	capturePath := filepath.Join(t.TempDir(), "captured-input")
	t.Setenv("COLLECTOR_BORG_CAPTURE", capturePath)
	system := defaultBorgSystem()
	baseLinkat := system.linkat
	linkCalls := 0
	system.linkat = func(oldDirFD int, oldPath string, newDirFD int, newPath string, flags int) error {
		linkCalls++
		return baseLinkat(oldDirFD, oldPath, newDirFD, newPath, flags)
	}
	backend, _ := newTestBorgBackendWithSystem(t, config, "capture-create-exact", system)
	if err := backend.Create(context.Background(), object); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if linkCalls != 1 {
		t.Fatalf("link calls = %d", linkCalls)
	}
	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(captured) != "encrypted-object-canary" {
		t.Fatalf("Borg read %q", captured)
	}
	assertNoBorgStages(t, config.WorkDir)
}

func TestBorgCreateRejectsEXDEVWithoutExecutingOrCopying(t *testing.T) {
	object := testPendingObject(t)
	config := testBorgConfig(t)
	system := defaultBorgSystem()
	system.linkat = func(int, string, int, string, int) error {
		return unix.EXDEV
	}
	backend, logPath := newTestBorgBackendWithSystem(t, config, "capture-create-exact", system)
	started := time.Now()
	err := backend.Create(context.Background(), object)
	if err == nil || err.Error() != "collector backend failed" {
		t.Fatalf("Create error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("EXDEV staging failure did not return promptly")
	}
	if _, statErr := os.Stat(logPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("EXDEV staging executed Borg: %v", statErr)
	}
	assertNoBorgStages(t, config.WorkDir)
}

func TestBorgStageEXDEVDoesNotInspectSourceFileDescriptor(t *testing.T) {
	stagePath := newPrivateDir(t)
	stageFD, err := unix.Open(stagePath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(stageFD)
	stage := &borgStage{
		system: defaultBorgSystem(),
		dirFD:  stageFD,
		fileFD: -1,
	}
	stage.system.linkat = func(int, string, int, string, int) error {
		return unix.EXDEV
	}
	source := &heldBorgInput{
		dirFD:  -1,
		fileFD: -1,
		name:   testDigestHex + ".tar.gz.age",
		size:   23,
	}
	started := time.Now()
	err = stage.materialize(context.Background(), source)
	if !errors.Is(err, unix.EXDEV) {
		t.Fatalf("materialize error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("EXDEV materialization did not return promptly")
	}
	entries, readErr := os.ReadDir(stagePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("EXDEV materialization created %d entries", len(entries))
	}
}

func TestBorgCreateDoesNotStageCancelledOrExpiredRequests(t *testing.T) {
	tests := []struct {
		name    string
		context func() context.Context
		want    string
	}{
		{
			name: "already cancelled",
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			want: "collector backend cancelled",
		},
		{
			name: "already expired",
			context: func() context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				cancel()
				return ctx
			},
			want: "collector backend timeout",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := testPendingObject(t)
			config := testBorgConfig(t)
			system := defaultBorgSystem()
			linkCalls := 0
			system.linkat = func(int, string, int, string, int) error {
				linkCalls++
				return errors.New("unexpected staging")
			}
			backend, logPath := newTestBorgBackendWithSystem(t, config, "exact", system)
			err := backend.Create(test.context(), object)
			if err == nil || err.Error() != test.want {
				t.Fatalf("Create error = %v, want %q", err, test.want)
			}
			if linkCalls != 0 {
				t.Fatalf("cancelled create made %d staging link calls", linkCalls)
			}
			if _, statErr := os.Stat(logPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("cancelled create executed Borg: %v", statErr)
			}
			assertNoBorgStages(t, config.WorkDir)
		})
	}
}

func TestBorgCreateTimeoutIncludesStagingLifecycle(t *testing.T) {
	object := testPendingObject(t)
	config := testBorgConfig(t)
	config.CreateTimeout = 100 * time.Millisecond
	system := defaultBorgSystem()
	baseLinkat := system.linkat
	system.linkat = func(oldDirFD int, oldPath string, newDirFD int, newPath string, flags int) error {
		time.Sleep(250 * time.Millisecond)
		return baseLinkat(oldDirFD, oldPath, newDirFD, newPath, flags)
	}
	backend, logPath := newTestBorgBackendWithSystem(t, config, "exact", system)
	err := backend.Create(context.Background(), object)
	if err == nil || err.Error() != "collector backend timeout" {
		t.Fatalf("Create error = %v", err)
	}
	if _, statErr := os.Stat(logPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expired staging executed Borg: %v", statErr)
	}
	assertNoBorgStages(t, config.WorkDir)
}

func TestBorgStageCancellationAfterLinkCleansDurably(t *testing.T) {
	object := testPendingObject(t)
	held, err := openBorgInput(object)
	if err != nil {
		t.Fatal(err)
	}
	defer held.close()
	config := testBorgConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	system := defaultBorgSystem()
	baseLinkat := system.linkat
	linked := false
	system.linkat = func(oldDirFD int, oldPath string, newDirFD int, newPath string, flags int) error {
		err := baseLinkat(oldDirFD, oldPath, newDirFD, newPath, flags)
		if err == nil {
			linked = true
			cancel()
		}
		return err
	}
	backend, _ := newTestBorgBackendWithSystem(t, config, "exact", system)
	stage, err := backend.stageInput(ctx, held)
	if stage != nil {
		t.Fatal("cancelled staging returned a stage")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stageInput error = %v", err)
	}
	if !linked {
		t.Fatal("test did not cancel after a successful link")
	}
	assertNoBorgStages(t, config.WorkDir)
	var stat unix.Stat_t
	if err := unix.Stat(object.EncryptedPath, &stat); err != nil {
		t.Fatal(err)
	}
	if stat.Nlink != 1 {
		t.Fatalf("source link count = %d", stat.Nlink)
	}
}

func TestNewBorgBackendRecoversOnlyValidCrashStages(t *testing.T) {
	t.Run("hardlink residue", func(t *testing.T) {
		config := testBorgConfig(t)
		stageName := ".sherpa-borg-0123456789abcdef0123456789abcdef.tmp"
		stagePath := filepath.Join(config.WorkDir, stageName)
		if err := os.Mkdir(stagePath, 0o700); err != nil {
			t.Fatal(err)
		}
		source := filepath.Join(newPrivateDir(t), testDigestHex+".tar.gz.age")
		if err := os.WriteFile(source, []byte("crash-residue"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(source, filepath.Join(stagePath, filepath.Base(source))); err != nil {
			t.Fatal(err)
		}
		if _, err := NewBorgBackend(config); err != nil {
			t.Fatalf("NewBorgBackend: %v", err)
		}
		if _, err := os.Lstat(stagePath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stage residue remains: %v", err)
		}
		var stat unix.Stat_t
		if err := unix.Stat(source, &stat); err != nil {
			t.Fatal(err)
		}
		if stat.Nlink != 1 {
			t.Fatalf("source link count = %d", stat.Nlink)
		}
	})

	t.Run("unexpected content fails closed", func(t *testing.T) {
		config := testBorgConfig(t)
		stagePath := filepath.Join(config.WorkDir, ".sherpa-borg-fedcba9876543210fedcba9876543210.tmp")
		if err := os.Mkdir(stagePath, 0o700); err != nil {
			t.Fatal(err)
		}
		unexpected := filepath.Join(stagePath, "unexpected")
		if err := os.WriteFile(unexpected, []byte("canary"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := NewBorgBackend(config)
		if err == nil || err.Error() != "collector backend unavailable" {
			t.Fatalf("NewBorgBackend error = %v", err)
		}
		if _, statErr := os.Lstat(unexpected); statErr != nil {
			t.Fatalf("unexpected content was mutated: %v", statErr)
		}
	})
}

func TestNewBorgBackendClosesCrashRecoveryDescriptorsOnFailure(t *testing.T) {
	config := testBorgConfig(t)
	stagePath := filepath.Join(config.WorkDir, ".sherpa-borg-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.tmp")
	if err := os.Mkdir(stagePath, 0o700); err != nil {
		t.Fatal(err)
	}
	fileName := strings.Repeat("a", 64) + ".tar.gz.age"
	if err := os.WriteFile(filepath.Join(stagePath, fileName), []byte("residue"), 0o600); err != nil {
		t.Fatal(err)
	}
	system := defaultBorgSystem()
	system.unlinkat = func(int, string, int) error { return unix.EIO }
	before := countOpenBorgTestDescriptors()
	for range 8 {
		if _, err := newBorgBackendWithSystem(config, time.Local, system); err == nil || err.Error() != "collector backend unavailable" {
			t.Fatalf("newBorgBackendWithSystem error = %v", err)
		}
	}
	after := countOpenBorgTestDescriptors()
	if after != before {
		t.Fatalf("open descriptor count grew from %d to %d", before, after)
	}
}

func TestBorgCreateSurfacesVerifiedStageCleanupFailure(t *testing.T) {
	object := testPendingObject(t)
	config := testBorgConfig(t)
	t.Setenv("COLLECTOR_BORG_CAPTURE", filepath.Join(t.TempDir(), "captured-input"))
	system := defaultBorgSystem()
	baseUnlinkat := system.unlinkat
	system.unlinkat = func(dirFD int, path string, flags int) error {
		if flags == 0 && path == object.DigestHex+".tar.gz.age" {
			return unix.EIO
		}
		return baseUnlinkat(dirFD, path, flags)
	}
	backend, _ := newTestBorgBackendWithSystem(t, config, "capture-create-exact", system)
	if err := backend.Create(context.Background(), object); err == nil || err.Error() != "collector backend failed" {
		t.Fatalf("Create error = %v", err)
	}
}

func TestBorgCommandsUseStableDescriptorFileOrder(t *testing.T) {
	object := testPendingObject(t)
	config := testBorgConfig(t)
	t.Setenv("COLLECTOR_BORG_CAPTURE", filepath.Join(t.TempDir(), "captured-input"))
	system := defaultBorgSystem()
	var mu sync.Mutex
	var bindings []struct {
		args       []string
		dir        string
		extraFiles int
		env        map[string]string
	}
	system.beforeStart = func(cmd *exec.Cmd) error {
		env := make(map[string]string)
		for _, item := range cmd.Env {
			key, value, ok := strings.Cut(item, "=")
			if ok && strings.HasPrefix(key, "BORG_") {
				env[key] = value
			}
		}
		mu.Lock()
		bindings = append(bindings, struct {
			args       []string
			dir        string
			extraFiles int
			env        map[string]string
		}{append([]string(nil), cmd.Args[1:]...), cmd.Dir, len(cmd.ExtraFiles), env})
		mu.Unlock()
		return nil
	}
	backend, _ := newTestBorgBackendWithSystem(t, config, "capture-create-exact", system)
	if err := backend.Create(context.Background(), object); err != nil {
		t.Fatalf("Create: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bindings) != 2 {
		t.Fatalf("bindings = %d", len(bindings))
	}
	for _, binding := range bindings {
		if binding.env["BORG_CACHE_DIR"] != borgFDPath(3) || binding.env["BORG_CONFIG_DIR"] != borgFDPath(4) || binding.env["BORG_SECURITY_DIR"] != borgFDPath(5) {
			t.Fatalf("state descriptor environment = %#v", binding.env)
		}
	}
	if bindings[0].extraFiles != 4 || bindings[0].dir != borgFDPath(6) {
		t.Fatalf("create binding = dir %q, extra files %d", bindings[0].dir, bindings[0].extraFiles)
	}
	if bindings[1].extraFiles != 3 || bindings[1].dir != "" {
		t.Fatalf("list binding = dir %q, extra files %d", bindings[1].dir, bindings[1].extraFiles)
	}
}

func TestBorgRunnerKillsAnchoredGroupBeforeWaitReapsLeader(t *testing.T) {
	config := testBorgConfig(t)
	system := defaultBorgSystem()
	baseKill := system.killProcessGroup
	baseWait := system.waitProcess
	var mu sync.Mutex
	var events []string
	system.killProcessGroup = func(pid int) error {
		mu.Lock()
		events = append(events, "kill")
		mu.Unlock()
		if !processExists(pid) {
			return errors.New("leader was reaped before group termination")
		}
		return baseKill(pid)
	}
	system.waitProcess = func(cmd *exec.Cmd) error {
		mu.Lock()
		events = append(events, "wait")
		mu.Unlock()
		return baseWait(cmd)
	}
	backend, _ := newTestBorgBackendWithSystem(t, config, "exact", system)
	if _, err := backend.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(events, []string{"kill", "wait"}) {
		t.Fatalf("runner events = %v", events)
	}
}

func TestBorgRunnerKillsDescendantsOnTimeoutAndOverflow(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process-group assertion targets supported runtime platforms")
	}
	for _, test := range []struct {
		name string
		mode string
	}{
		{name: "timeout", mode: "timeout-tree"},
		{name: "overflow", mode: "overflow-tree"},
	} {
		t.Run(test.name, func(t *testing.T) {
			pidPath := filepath.Join(t.TempDir(), "child-pid")
			t.Setenv("COLLECTOR_BORG_CHILD_PID", pidPath)
			backend, _ := newTestBorgBackend(t, test.mode)
			if test.mode == "timeout-tree" {
				backend.config.QueryTimeout = 50 * time.Millisecond
			}
			_, err := backend.List(context.Background())
			want := "collector backend failed"
			if test.mode == "timeout-tree" {
				want = "collector backend timeout"
			}
			if err == nil || err.Error() != want {
				t.Fatalf("List error = %v, want %q", err, want)
			}
			assertProcessExited(t, waitForChildPID(t, pidPath))
		})
	}
}

func TestNewBorgBackendRejectsOpenSSHTokenAndBorgRSHSeparatorPaths(t *testing.T) {
	bad := []string{"%h", "%%", "space path", "tab\tpath", "quote'path", "double\"path", "back\\slash", "line\npath", "colon:path", "plus+path", "equals=path", "comma,path", "at@path"}
	for _, suffix := range bad {
		t.Run(strings.ReplaceAll(suffix, "/", "-"), func(t *testing.T) {
			for _, field := range []string{"key", "known-hosts"} {
				config := testBorgConfig(t)
				path := "/run/collector/" + suffix
				if field == "key" {
					config.SSHKeyFile = path
				} else {
					config.KnownHostsFile = path
				}
				_, err := NewBorgBackend(config)
				if err == nil || err.Error() != "collector backend unavailable" {
					t.Fatalf("%s path %q error = %v", field, path, err)
				}
				if strings.Contains(err.Error(), suffix) {
					t.Fatalf("error exposed path token %q", suffix)
				}
			}
		})
	}

	config := testBorgConfig(t)
	config.SSHKeyFile = "/run/collector/id_ed25519-01.key"
	config.KnownHostsFile = "/run/collector/known_hosts-01.txt"
	if _, err := NewBorgBackend(config); err != nil {
		t.Fatalf("valid fixed deployment paths rejected: %v", err)
	}
}

func TestBorgStateDescriptorsSurviveRootAndChildABA(t *testing.T) {
	tests := []struct {
		name      string
		stateName string
		swap      func(testing.TB, string) func()
	}{
		{
			name:      "root",
			stateName: "cache",
			swap: func(t testing.TB, root string) func() {
				backup := root + ".original"
				if err := os.Rename(root, backup); err != nil {
					t.Fatal(err)
				}
				createBorgWorkTree(t, root)
				return func() {
					if err := os.RemoveAll(root); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(backup, root); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name:      "cache child",
			stateName: "cache",
			swap:      swapBorgStateChild("cache"),
		},
		{
			name:      "config child",
			stateName: "config",
			swap:      swapBorgStateChild("config"),
		},
		{
			name:      "security child",
			stateName: "security",
			swap:      swapBorgStateChild("security"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testBorgConfig(t)
			markerReady := filepath.Join(t.TempDir(), "marker-ready")
			helperRelease := filepath.Join(t.TempDir(), "helper-release")
			t.Setenv("COLLECTOR_BORG_READ_READY", markerReady)
			t.Setenv("COLLECTOR_BORG_HELPER_RELEASE", helperRelease)
			t.Setenv("COLLECTOR_BORG_STATE_NAME", test.stateName)
			bound := make(chan struct{})
			start := make(chan struct{})
			system := defaultBorgSystem()
			system.beforeStart = func(*exec.Cmd) error {
				close(bound)
				<-start
				return nil
			}
			backend, _ := newTestBorgBackendWithSystem(t, config, "state-marker", system)
			done := make(chan error, 1)
			go func() {
				_, err := backend.List(context.Background())
				done <- err
			}()
			<-bound
			restore := test.swap(t, config.WorkDir)
			if runtime.GOOS == "darwin" {
				restore()
				restore = nil
			}
			close(start)
			waitForFile(t, markerReady)
			if restore != nil {
				restore()
			}
			if err := os.WriteFile(helperRelease, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatalf("List: %v", err)
			}
			marker := filepath.Join(config.WorkDir, test.stateName, "binding-marker")
			data, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "descriptor-bound" {
				t.Fatalf("marker = %q", data)
			}
		})
	}
}

func TestBorgStateReplacementBeforeCommandFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		replace func(testing.TB, string)
	}{
		{
			name: "root",
			replace: func(t testing.TB, root string) {
				if err := os.Rename(root, root+".original"); err != nil {
					t.Fatal(err)
				}
				createBorgWorkTree(t, root)
			},
		},
		{name: "cache child", replace: replaceBorgStateChild("cache")},
		{name: "config child", replace: replaceBorgStateChild("config")},
		{name: "security child", replace: replaceBorgStateChild("security")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testBorgConfig(t)
			backend, logPath := newTestBorgBackendWithSystem(t, config, "exact", defaultBorgSystem())
			test.replace(t, config.WorkDir)
			_, err := backend.List(context.Background())
			if err == nil || err.Error() != "collector backend failed" {
				t.Fatalf("List error = %v", err)
			}
			if _, statErr := os.Stat(logPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("replacement state executed Borg: %v", statErr)
			}
		})
	}
}

func newTestBorgBackendWithSystem(t testing.TB, config BorgConfig, mode string, system borgSystem) (*BorgBackend, string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "borg-invocations.jsonl")
	t.Setenv("COLLECTOR_BORG_HELPER", "1")
	t.Setenv("COLLECTOR_BORG_MODE", mode)
	t.Setenv("COLLECTOR_BORG_LOG", logPath)
	backend, err := newBorgBackendWithSystem(config, time.Local, system)
	if err != nil {
		t.Fatalf("newBorgBackendWithSystem: %v", err)
	}
	return backend, logPath
}

func countOpenBorgTestDescriptors() int {
	count := 0
	for fd := 0; fd < 4096; fd++ {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			count++
		}
	}
	return count
}

func waitForFile(t testing.TB, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file did not appear: %s", path)
}

func assertNoBorgStages(t testing.TB, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".sherpa-borg-") {
			t.Fatalf("staging residue remains: %s", entry.Name())
		}
	}
}

func replaceBorgStateChild(name string) func(testing.TB, string) {
	return func(t testing.TB, root string) {
		path := filepath.Join(root, name)
		if err := os.Rename(path, path+".original"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func swapBorgStateChild(name string) func(testing.TB, string) func() {
	return func(t testing.TB, root string) func() {
		path := filepath.Join(root, name)
		backup := path + ".original"
		if err := os.Rename(path, backup); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		return func() {
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(backup, path); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func createBorgWorkTree(t testing.TB, root string) {
	t.Helper()
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cache", "config", "security"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}
