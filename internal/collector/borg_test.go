package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	borgTestRepository = "ssh://borg-private-canary@example.invalid/./repository-canary"
	borgTestSSHKey     = "/private/collector/key-canary"
	borgTestKnownHosts = "/private/collector/known-hosts-canary"
)

type borgInvocation struct {
	Args []string          `json:"args"`
	Dir  string            `json:"dir"`
	Env  map[string]string `json:"env"`
}

func TestMain(m *testing.M) {
	if os.Getenv("COLLECTOR_BORG_HELPER") == "1" {
		os.Exit(runBorgTestHelper())
	}
	os.Exit(m.Run())
}

func TestBorgCreateBuildsFixedArgumentsWithoutShell(t *testing.T) {
	backend, logPath := newTestBorgBackend(t, "exact")
	object := testPendingObject(t)

	if err := backend.Create(context.Background(), object); err != nil {
		t.Fatalf("Create: %v", err)
	}
	calls := readBorgInvocations(t, logPath)
	if len(calls) != 2 {
		t.Fatalf("invocations = %d, want create and verification list", len(calls))
	}
	if want := []string{"create", "--compression", "none", "::sherpa-" + testDigestHex, testDigestHex + ".tar.gz.age"}; !reflect.DeepEqual(calls[0].Args, want) {
		t.Fatalf("create args = %#v, want %#v", calls[0].Args, want)
	}
	if want := []string{"list", "--json"}; !reflect.DeepEqual(calls[1].Args, want) {
		t.Fatalf("list args = %#v, want %#v", calls[1].Args, want)
	}
	if base := filepath.Base(calls[0].Dir); !borgStageNamePattern.MatchString(base) {
		t.Fatalf("create directory = %q", calls[0].Dir)
	}
	if strings.Contains(calls[0].Dir, filepath.Dir(object.EncryptedPath)) {
		t.Fatalf("create followed source parent path: %q", calls[0].Dir)
	}
	for _, arg := range calls[0].Args {
		if strings.ContainsAny(arg, ";|&$`\n") {
			t.Fatalf("shell-significant argument = %q", arg)
		}
	}
}

func TestBorgCreatePassesPrivateValuesOnlyThroughEnvironment(t *testing.T) {
	t.Setenv("BORG_REPO", "inherited-repository-canary")
	t.Setenv("BORG_PASSPHRASE", "inherited-passphrase-canary")
	t.Setenv("BORG_REMOTE_PATH", "inherited-remote-path-canary")
	backend, logPath := newTestBorgBackend(t, "exact")

	if err := backend.Create(context.Background(), testPendingObject(t)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	calls := readBorgInvocations(t, logPath)
	for _, call := range calls {
		joined := strings.Join(call.Args, " ")
		for _, private := range []string{borgTestRepository, borgTestSSHKey, borgTestKnownHosts, backend.config.WorkDir} {
			if strings.Contains(joined, private) {
				t.Fatalf("argv exposed private value %q: %#v", private, call.Args)
			}
		}
		wantKeys := []string{"BORG_CACHE_DIR", "BORG_CONFIG_DIR", "BORG_REPO", "BORG_RSH", "BORG_SECURITY_DIR", "BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK"}
		gotKeys := make([]string, 0, len(call.Env))
		for key := range call.Env {
			gotKeys = append(gotKeys, key)
		}
		sort.Strings(gotKeys)
		if !reflect.DeepEqual(gotKeys, wantKeys) {
			t.Fatalf("Borg environment keys = %v, want %v", gotKeys, wantKeys)
		}
		if call.Env["BORG_REPO"] != borgTestRepository || call.Env["BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK"] != "yes" {
			t.Fatalf("Borg environment = %#v", call.Env)
		}
		for _, inherited := range []string{"inherited-repository-canary", "inherited-passphrase-canary", "inherited-remote-path-canary"} {
			for _, value := range call.Env {
				if strings.Contains(value, inherited) {
					t.Fatalf("inherited Borg value survived filtering: %q", inherited)
				}
			}
		}
	}
}

func TestBorgCreateUsesPinnedKnownHostsAndStrictChecking(t *testing.T) {
	backend, logPath := newTestBorgBackend(t, "exact")
	if err := backend.Create(context.Background(), testPendingObject(t)); err != nil {
		t.Fatal(err)
	}
	got := readBorgInvocations(t, logPath)[0].Env["BORG_RSH"]
	want := "ssh -i " + borgTestSSHKey + " -o IdentitiesOnly=yes -o UserKnownHostsFile=" + borgTestKnownHosts + " -o StrictHostKeyChecking=yes -p 23"
	if got != want {
		t.Fatalf("BORG_RSH = %q, want %q", got, want)
	}
	for _, forbidden := range []string{"StrictHostKeyChecking=no", "accept-new", "UserKnownHostsFile=/dev/null"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("BORG_RSH weakens host verification: %q", got)
		}
	}
}

func TestBorgExistsUsesExactJSONNameEquality(t *testing.T) {
	backend, logPath := newTestBorgBackend(t, "exact")
	exists, err := backend.Exists(context.Background(), "sha256:"+testDigestHex)
	if err != nil || !exists {
		t.Fatalf("Exists = %v, %v", exists, err)
	}
	calls := readBorgInvocations(t, logPath)
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].Args, []string{"list", "--json"}) {
		t.Fatalf("invocations = %#v", calls)
	}
}

func TestBorgExistsRejectsPrefixSuffixAndGlobMatches(t *testing.T) {
	backend, _ := newTestBorgBackend(t, "nonmatches")
	exists, err := backend.Exists(context.Background(), "sha256:"+testDigestHex)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Fatal("non-exact archive name was accepted")
	}
}

func TestBorgRunnerBoundsOutput(t *testing.T) {
	backend, _ := newTestBorgBackend(t, "overflow")
	started := time.Now()
	_, err := backend.List(context.Background())
	if err == nil || err.Error() != "collector backend failed" {
		t.Fatalf("List error = %v", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("bounded output failure hung")
	}
}

func TestBorgRunnerTimeoutAndCancellation(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		backend, _ := newTestBorgBackend(t, "sleep")
		backend.config.QueryTimeout = 50 * time.Millisecond
		_, err := backend.List(context.Background())
		if err == nil || err.Error() != "collector backend timeout" {
			t.Fatalf("List error = %v", err)
		}
	})
	t.Run("parent deadline", func(t *testing.T) {
		backend, _ := newTestBorgBackend(t, "sleep")
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := backend.List(ctx)
		if err == nil || err.Error() != "collector backend timeout" {
			t.Fatalf("List error = %v", err)
		}
	})
	t.Run("cancellation reaps process tree", func(t *testing.T) {
		if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			t.Skip("process-group assertion targets supported runtime platforms")
		}
		pidPath := filepath.Join(t.TempDir(), "child-pid")
		t.Setenv("COLLECTOR_BORG_CHILD_PID", pidPath)
		backend, _ := newTestBorgBackend(t, "tree")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := backend.List(ctx)
			done <- err
		}()
		childPID := waitForChildPID(t, pidPath)
		cancel()
		select {
		case err := <-done:
			if err == nil || err.Error() != "collector backend cancelled" {
				t.Fatalf("List error = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("cancelled Borg command was not reaped")
		}
		deadline := time.Now().Add(3 * time.Second)
		for processExists(childPID) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if processExists(childPID) {
			t.Fatalf("descendant process %d survived cancellation", childPID)
		}
	})
}

func TestBorgErrorsClassifyWithoutCommandOutputOrPrivateValues(t *testing.T) {
	backend, _ := newTestBorgBackend(t, "failure")
	_, err := backend.List(context.Background())
	if err == nil || err.Error() != "collector backend failed" {
		t.Fatalf("List error = %v", err)
	}
	for _, canary := range []string{"stdout-private-canary", "stderr-private-canary", borgTestRepository, borgTestSSHKey, borgTestKnownHosts, backend.config.WorkDir, backend.config.Binary} {
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("error exposed private value %q: %v", canary, err)
		}
	}
}

func TestBorgCreateValidatesPendingObjectBeforeExecution(t *testing.T) {
	valid := testPendingObject(t)
	cases := map[string]func(*PendingObject){
		"object ID": func(object *PendingObject) { object.ObjectID = object.DigestHex },
		"digest":    func(object *PendingObject) { object.DigestHex = strings.Repeat("f", 64) },
		"archive":   func(object *PendingObject) { object.ArchiveName += "-extra" },
		"basename": func(object *PendingObject) {
			object.EncryptedPath = filepath.Join(filepath.Dir(object.EncryptedPath), "other.tar.gz.age")
		},
		"relative path": func(object *PendingObject) { object.EncryptedPath = object.DigestHex + ".tar.gz.age" },
		"unclean path": func(object *PendingObject) {
			object.EncryptedPath = filepath.Dir(object.EncryptedPath) + string(os.PathSeparator) + "." + string(os.PathSeparator) + filepath.Base(object.EncryptedPath)
		},
		"uppercase name": func(object *PendingObject) { object.ArchiveName = strings.ToUpper(object.ArchiveName) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			backend, logPath := newTestBorgBackend(t, "exact")
			object := valid
			mutate(&object)
			err := backend.Create(context.Background(), object)
			if err == nil || err.Error() != "collector backend failed" {
				t.Fatalf("Create error = %v", err)
			}
			if _, statErr := os.Stat(logPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid object executed Borg: %v", statErr)
			}
		})
	}
}

func TestBorgCreateRequiresExactPostCreateVerification(t *testing.T) {
	backend, _ := newTestBorgBackend(t, "nonmatches")
	err := backend.Create(context.Background(), testPendingObject(t))
	if err == nil || err.Error() != "collector remote verification failed" {
		t.Fatalf("Create error = %v", err)
	}
}

func TestBorgListParsesTimestampsAndSortsDeterministically(t *testing.T) {
	backend, _ := newTestBorgBackend(t, "list")
	archives, err := backend.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	assertArchiveNames(t, archives, []string{"sherpa-a", "sherpa-b", "sherpa-c"})
	if !archives[0].StartedAt.Equal(time.Date(2024, 1, 2, 3, 4, 5, 123456000, time.Local)) || archives[0].StartedAt.Location() != time.Local {
		t.Fatalf("zone-less start = %v in %v", archives[0].StartedAt, archives[0].StartedAt.Location())
	}
	_, offset := archives[1].StartedAt.Zone()
	if !archives[1].StartedAt.Equal(time.Date(2024, 1, 2, 4, 4, 5, 0, time.UTC)) || offset != 60*60 {
		t.Fatalf("offset start = %v with offset %d", archives[1].StartedAt, offset)
	}
	if !archives[2].StartedAt.Equal(time.Date(2024, 1, 3, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("UTC start = %v", archives[2].StartedAt)
	}
}

func TestBorgListRejectsMalformedOrUnknownShape(t *testing.T) {
	for _, mode := range []string{"malformed", "missing-archives", "null-archives", "wrong-archives", "missing-name", "bad-start", "trailing-json"} {
		t.Run(mode, func(t *testing.T) {
			backend, _ := newTestBorgBackend(t, mode)
			_, err := backend.List(context.Background())
			if err == nil || err.Error() != "collector backend failed" {
				t.Fatalf("List error = %v", err)
			}
		})
	}
}

func TestNewBorgBackendRejectsInvalidConfigurationSafely(t *testing.T) {
	valid := testBorgConfig(t)
	cases := map[string]func(*BorgConfig){
		"binary":         func(config *BorgConfig) { config.Binary = "" },
		"repository":     func(config *BorgConfig) { config.Repository = "" },
		"SSH key":        func(config *BorgConfig) { config.SSHKeyFile = "" },
		"known hosts":    func(config *BorgConfig) { config.KnownHostsFile = "" },
		"work directory": func(config *BorgConfig) { config.WorkDir = "relative" },
		"create timeout": func(config *BorgConfig) { config.CreateTimeout = 0 },
		"query timeout":  func(config *BorgConfig) { config.QueryTimeout = -time.Second },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			config := valid
			mutate(&config)
			_, err := NewBorgBackend(config)
			if err == nil || err.Error() != "collector backend unavailable" {
				t.Fatalf("NewBorgBackend error = %v", err)
			}
		})
	}
}

func TestNewBorgBackendRejectsSymlinkWorkDirectoryWithoutChangingTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "work-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	config := testBorgConfig(t)
	config.WorkDir = link
	_, err := NewBorgBackend(config)
	if err == nil || err.Error() != "collector backend unavailable" {
		t.Fatalf("NewBorgBackend error = %v", err)
	}
	info, statErr := os.Stat(target)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("rejected symlink target mode changed to %04o", info.Mode().Perm())
	}
}

func newTestBorgBackend(t testing.TB, mode string) (*BorgBackend, string) {
	t.Helper()
	return newTestBorgBackendForObject(t, mode, "")
}

func newTestBorgBackendForObject(t testing.TB, mode, objectPath string) (*BorgBackend, string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "borg-invocations.jsonl")
	t.Setenv("COLLECTOR_BORG_HELPER", "1")
	t.Setenv("COLLECTOR_BORG_MODE", mode)
	t.Setenv("COLLECTOR_BORG_LOG", logPath)
	t.Setenv("COLLECTOR_BORG_OBJECT_PATH", objectPath)
	backend, err := NewBorgBackend(testBorgConfig(t))
	if err != nil {
		t.Fatalf("NewBorgBackend: %v", err)
	}
	return backend, logPath
}

func testBorgConfig(t testing.TB) BorgConfig {
	t.Helper()
	return BorgConfig{
		Binary:         os.Args[0],
		Repository:     borgTestRepository,
		SSHKeyFile:     borgTestSSHKey,
		KnownHostsFile: borgTestKnownHosts,
		WorkDir:        newPrivateDir(t),
		CreateTimeout:  2 * time.Second,
		QueryTimeout:   2 * time.Second,
	}
}

func testPendingObject(t testing.TB) PendingObject {
	t.Helper()
	dir := newPrivateDir(t)
	path := filepath.Join(dir, testDigestHex+".tar.gz.age")
	if err := os.WriteFile(path, []byte("encrypted-object-canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	return PendingObject{
		ObjectID:      "sha256:" + testDigestHex,
		DigestHex:     testDigestHex,
		ArchiveName:   "sherpa-" + testDigestHex,
		EncryptedPath: path,
		EncryptedSize: 23,
		ReceivedAt:    time.Now(),
	}
}

func readBorgInvocations(t testing.TB, path string) []borgInvocation {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var calls []borgInvocation
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var call borgInvocation
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatalf("decode invocation: %v", err)
		}
		calls = append(calls, call)
	}
	return calls
}

func runBorgTestHelper() int {
	if err := recordBorgInvocation(); err != nil {
		return 97
	}
	mode := os.Getenv("COLLECTOR_BORG_MODE")
	action := ""
	if len(os.Args) > 1 {
		action = os.Args[1]
	}
	if action == "create" {
		switch mode {
		case "capture-create-exact":
			data, err := os.ReadFile(os.Args[len(os.Args)-1])
			if err != nil {
				return 92
			}
			if err := os.WriteFile(os.Getenv("COLLECTOR_BORG_CAPTURE"), data, 0o600); err != nil {
				return 91
			}
			if err := signalBorgTestHelperAndWait(); err != nil {
				return 90
			}
		case "create-failure-exact", "create-failure-absent", "create-failure-list-failure":
			fmt.Fprintln(os.Stdout, "stdout-private-canary")
			fmt.Fprintln(os.Stderr, "stderr-private-canary")
			return 9
		case "create-timeout-exact":
			time.Sleep(30 * time.Second)
		case "create-overflow-exact":
			_, _ = os.Stdout.Write([]byte(strings.Repeat("x", 2<<20)))
		case "create-waitdelay-exact":
			child := exec.Command("sleep", "30")
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
			if err := child.Start(); err != nil {
				return 96
			}
		case "replace-during-create-exact":
			if err := replaceBorgTestObject(); err != nil {
				return 93
			}
		}
		return 0
	}
	switch mode {
	case "exact", "capture-create-exact", "create-failure-exact", "create-timeout-exact", "create-overflow-exact", "create-waitdelay-exact", "replace-during-create-exact":
		fmt.Printf(`{"archives":[{"name":"sherpa-%s","start":"2024-01-02T03:04:05.123456Z"}],"repository":{}}`, testDigestHex)
	case "state-marker":
		stateEnvironment := map[string]string{
			"cache":    "BORG_CACHE_DIR",
			"config":   "BORG_CONFIG_DIR",
			"security": "BORG_SECURITY_DIR",
		}
		stateName := os.Getenv("COLLECTOR_BORG_STATE_NAME")
		if stateName == "" {
			stateName = "cache"
		}
		stateKey, ok := stateEnvironment[stateName]
		if !ok {
			return 87
		}
		if err := os.WriteFile(filepath.Join(os.Getenv(stateKey), "binding-marker"), []byte("descriptor-bound"), 0o600); err != nil {
			return 89
		}
		if err := signalBorgTestHelperAndWait(); err != nil {
			return 88
		}
		fmt.Print(`{"archives":[]}`)
	case "replace-during-list-exact":
		if err := replaceBorgTestObject(); err != nil {
			return 93
		}
		fmt.Printf(`{"archives":[{"name":"sherpa-%s","start":"2024-01-02T03:04:05.123456Z"}]}`, testDigestHex)
	case "create-failure-absent":
		fmt.Print(`{"archives":[]}`)
	case "create-failure-list-failure":
		return 8
	case "create-success-list-sleep":
		time.Sleep(30 * time.Second)
	case "nonmatches":
		fmt.Printf(`{"archives":[{"name":"xsherpa-%[1]s","start":"2024-01-02T03:04:05Z"},{"name":"sherpa-%[1]s-extra","start":"2024-01-02T03:04:05Z"},{"name":"sherpa-*","start":"2024-01-02T03:04:05Z"}]}`, testDigestHex)
	case "overflow":
		_, _ = os.Stdout.Write([]byte(strings.Repeat("x", 2<<20)))
	case "sleep":
		time.Sleep(30 * time.Second)
	case "timeout-tree":
		child := exec.Command("sleep", "30")
		if err := startBorgTestDescendant(child); err != nil {
			return 96
		}
		_ = child.Wait()
	case "overflow-tree":
		child := exec.Command("sleep", "30")
		if err := startBorgTestDescendant(child); err != nil {
			return 96
		}
		_, _ = os.Stdout.Write([]byte(strings.Repeat("x", 2<<20)))
		time.Sleep(30 * time.Second)
	case "tree":
		child := exec.Command("sleep", "30")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := startBorgTestDescendant(child); err != nil {
			return 96
		}
		_ = child.Wait()
	case "tree-holds-pipe":
		child := exec.Command("sleep", "30")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := startBorgTestDescendant(child); err != nil {
			return 96
		}
	case "tree-closed-stdio":
		if err := startBorgTestDescendant(exec.Command("sleep", "30")); err != nil {
			return 96
		}
	case "tree-success":
		if err := startBorgTestDescendant(exec.Command("sleep", "30")); err != nil {
			return 96
		}
		fmt.Print(`{"archives":[]}`)
	case "failure":
		fmt.Fprintln(os.Stdout, "stdout-private-canary", borgTestRepository)
		fmt.Fprintln(os.Stderr, "stderr-private-canary", borgTestSSHKey, borgTestKnownHosts)
		return 8
	case "list":
		fmt.Print(`{"archives":[{"name":"sherpa-c","start":"2024-01-03T03:04:05Z"},{"name":"sherpa-b","start":"2024-01-02T05:04:05+01:00"},{"name":"sherpa-a","start":"2024-01-02T03:04:05.123456"}],"cache":{},"encryption":{},"repository":{}}`)
	case "local-list":
		fmt.Print(`{"archives":[{"name":"sherpa-zoned-one","start":"2024-01-15T13:00:00-08:00"},{"name":"sherpa-local-noon","start":"2024-01-15T12:00:00"}]}`)
	case "malformed":
		fmt.Print(`{"archives":[`)
	case "missing-archives":
		fmt.Print(`{"repository":{}}`)
	case "null-archives":
		fmt.Print(`{"archives":null}`)
	case "wrong-archives":
		fmt.Print(`{"archives":{}}`)
	case "missing-name":
		fmt.Print(`{"archives":[{"start":"2024-01-02T03:04:05Z"}]}`)
	case "bad-start":
		fmt.Print(`{"archives":[{"name":"sherpa-a","start":"not-a-time"}]}`)
	case "trailing-json":
		fmt.Print(`{"archives":[]} {"archives":[]}`)
	default:
		return 94
	}
	return 0
}

func startBorgTestDescendant(child *exec.Cmd) error {
	if err := child.Start(); err != nil {
		return err
	}
	if err := os.WriteFile(os.Getenv("COLLECTOR_BORG_CHILD_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		_ = child.Process.Kill()
		return err
	}
	return nil
}

func signalBorgTestHelperAndWait() error {
	ready := os.Getenv("COLLECTOR_BORG_READ_READY")
	if ready == "" {
		return nil
	}
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		return err
	}
	release := os.Getenv("COLLECTOR_BORG_HELPER_RELEASE")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(release); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("helper release timeout")
}

func replaceBorgTestObject() error {
	path := os.Getenv("COLLECTOR_BORG_OBJECT_PATH")
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := os.Rename(path, path+".original"); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Repeat("r", int(info.Size()))), 0o600)
}

func recordBorgInvocation() error {
	file, err := os.OpenFile(os.Getenv("COLLECTOR_BORG_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	env := make(map[string]string)
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok && strings.HasPrefix(key, "BORG_") {
			env[key] = value
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	return json.NewEncoder(file).Encode(borgInvocation{Args: os.Args[1:], Dir: dir, Env: env})
}

func waitForChildPID(t testing.TB, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(string(data))
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper child did not start")
	return 0
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
