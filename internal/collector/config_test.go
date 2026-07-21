package collector

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"sherpa/internal/recoveryarchive"
)

const (
	testToken      = "collector-token-canary-6d42"
	testRepository = "ssh://borg-canary@example.invalid/./private-repository-canary"
	testSSHKey     = "private-ssh-key-canary-91ee"
	testKnownHosts = "known-hosts-canary ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICanary"
)

func TestLoadConfigAcceptsCompleteFileBackedConfiguration(t *testing.T) {
	env, recipient := completeConfigEnv(t)
	env["SHERPA_COLLECTOR_LISTEN_ADDR"] = "127.0.0.1:9090"
	env["SHERPA_COLLECTOR_SPOOL_DIR"] = "/collector/spool"
	env["SHERPA_COLLECTOR_STATE_DIR"] = "/collector/state"
	env["SHERPA_COLLECTOR_BORG_DIR"] = "/collector/borg"
	env["SHERPA_COLLECTOR_MAX_BYTES"] = "1048576"
	env["SHERPA_COLLECTOR_MAX_UNCOMPRESSED_BYTES"] = "2097152"
	env["SHERPA_COLLECTOR_MAX_MEMBERS"] = "42"
	env["SHERPA_COLLECTOR_RETRY_INTERVAL"] = "30s"
	env["SHERPA_COLLECTOR_PARTIAL_MAX_AGE"] = "2h"
	env["SHERPA_COLLECTOR_STARTUP_GRACE"] = "4h"
	env["SHERPA_COLLECTOR_MAX_RECOVERY_AGE"] = "3h"

	cfg, err := LoadConfig(mapGetenv(env))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	wantDigest := sha256.Sum256([]byte(testToken))
	if cfg.TokenDigest != wantDigest {
		t.Fatal("TokenDigest does not match the trimmed token")
	}
	if cfg.AgeRecipient == nil || cfg.AgeRecipient.(interface{ String() string }).String() != recipient {
		t.Fatalf("AgeRecipient = %v, want generated public recipient", cfg.AgeRecipient)
	}
	if cfg.ListenAddr != "127.0.0.1:9090" || cfg.SpoolDir != "/collector/spool" || cfg.StateDir != "/collector/state" {
		t.Fatalf("collector paths/listen = %#v", cfg)
	}
	if cfg.Limits.MaxCompressedBytes != 1_048_576 || cfg.Limits.MaxUncompressedBytes != 2_097_152 || cfg.Limits.MaxMembers != 42 {
		t.Fatalf("limits = %#v", cfg.Limits)
	}
	if cfg.RetryInterval != 30*time.Second || cfg.PartialMaxAge != 2*time.Hour || cfg.StartupGrace != 4*time.Hour || cfg.MaxRecoveryAge != 3*time.Hour {
		t.Fatalf("durations = retry %v partial %v grace %v recovery %v", cfg.RetryInterval, cfg.PartialMaxAge, cfg.StartupGrace, cfg.MaxRecoveryAge)
	}
	if cfg.Borg.Binary != "borg" || cfg.Borg.Repository != testRepository || cfg.Borg.WorkDir != "/collector/borg" {
		t.Fatalf("Borg basic config = %#v", cfg.Borg)
	}
	if cfg.Borg.SSHKeyFile != env["SHERPA_COLLECTOR_BORG_SSH_KEY_FILE"] || cfg.Borg.KnownHostsFile != env["SHERPA_COLLECTOR_KNOWN_HOSTS_FILE"] {
		t.Fatalf("Borg file config = %#v", cfg.Borg)
	}
	if cfg.Borg.CreateTimeout != 10*time.Minute || cfg.Borg.QueryTimeout != 2*time.Minute {
		t.Fatalf("Borg timeouts = create %v query %v", cfg.Borg.CreateTimeout, cfg.Borg.QueryTimeout)
	}
}

func TestLoadConfigAppliesDocumentedDefaults(t *testing.T) {
	env, _ := completeConfigEnv(t)

	cfg, err := LoadConfig(mapGetenv(env))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	wantLimits := recoveryarchive.DefaultLimits()
	if cfg.ListenAddr != ":8080" || cfg.SpoolDir != "/data/spool" || cfg.StateDir != "/data/state" {
		t.Fatalf("defaults = %#v", cfg)
	}
	if cfg.Borg.WorkDir != "/data/borg" || cfg.Borg.Binary != "borg" || cfg.Borg.CreateTimeout != 10*time.Minute || cfg.Borg.QueryTimeout != 2*time.Minute {
		t.Fatalf("Borg defaults = %#v", cfg.Borg)
	}
	if cfg.Limits != wantLimits {
		t.Fatalf("Limits = %#v, want %#v", cfg.Limits, wantLimits)
	}
	if cfg.RetryInterval != 5*time.Minute || cfg.PartialMaxAge != 24*time.Hour || cfg.StartupGrace != 26*time.Hour || cfg.MaxRecoveryAge != 26*time.Hour {
		t.Fatalf("duration defaults = retry %v partial %v grace %v recovery %v", cfg.RetryInterval, cfg.PartialMaxAge, cfg.StartupGrace, cfg.MaxRecoveryAge)
	}
}

func TestLoadConfigRejectsEveryPartialConfiguration(t *testing.T) {
	for _, key := range []string{
		"SHERPA_COLLECTOR_TOKEN_FILE",
		"SHERPA_COLLECTOR_AGE_RECIPIENT_FILE",
		"SHERPA_COLLECTOR_BORG_REPOSITORY_FILE",
		"SHERPA_COLLECTOR_BORG_SSH_KEY_FILE",
		"SHERPA_COLLECTOR_KNOWN_HOSTS_FILE",
	} {
		t.Run(key, func(t *testing.T) {
			env, _ := completeConfigEnv(t)
			env[key] = filepath.Join(t.TempDir(), "missing-setting-canary")
			_, err := LoadConfig(mapGetenv(env))
			assertSafeSettingError(t, err, key)
		})
	}
}

func TestLoadConfigRejectsMissingEmptySymlinkAndNonRegularSecretFiles(t *testing.T) {
	secretKeys := []string{
		"SHERPA_COLLECTOR_TOKEN_FILE",
		"SHERPA_COLLECTOR_BORG_REPOSITORY_FILE",
		"SHERPA_COLLECTOR_BORG_SSH_KEY_FILE",
	}
	for _, key := range secretKeys {
		key := key
		t.Run(key, func(t *testing.T) {
			cases := map[string]func(testing.TB) string{
				"missing": func(t testing.TB) string {
					return filepath.Join(t.TempDir(), "missing-secret-canary")
				},
				"empty": func(t testing.TB) string {
					return writeTestFile(t, "empty-secret", "", 0o600)
				},
				"symlink": func(t testing.TB) string {
					target := writeTestFile(t, "symlink-target", "secret-symlink-target-canary", 0o600)
					link := filepath.Join(t.TempDir(), "secret-link-canary")
					if err := os.Symlink(target, link); err != nil {
						t.Fatal(err)
					}
					return link
				},
				"non-regular": func(t testing.TB) string {
					return t.TempDir()
				},
			}
			for name, makePath := range cases {
				t.Run(name, func(t *testing.T) {
					env, _ := completeConfigEnv(t)
					env[key] = makePath(t)
					_, err := LoadConfig(mapGetenv(env))
					assertSafeSettingError(t, err, key)
				})
			}
		})
	}
}

func TestLoadConfigRejectsMissingEmptySymlinkAndNonRegularPublicFiles(t *testing.T) {
	for _, key := range []string{"SHERPA_COLLECTOR_AGE_RECIPIENT_FILE", "SHERPA_COLLECTOR_KNOWN_HOSTS_FILE"} {
		key := key
		t.Run(key, func(t *testing.T) {
			for _, test := range []struct {
				name     string
				makePath func(testing.TB) string
			}{
				{name: "missing", makePath: func(t testing.TB) string { return filepath.Join(t.TempDir(), "missing-public-canary") }},
				{name: "empty", makePath: func(t testing.TB) string { return writeTestFile(t, "empty-public", "", 0o644) }},
				{name: "symlink", makePath: func(t testing.TB) string {
					target := writeTestFile(t, "public-target", "public-symlink-target-canary", 0o644)
					link := filepath.Join(t.TempDir(), "public-link-canary")
					if err := os.Symlink(target, link); err != nil {
						t.Fatal(err)
					}
					return link
				}},
				{name: "non-regular", makePath: func(t testing.TB) string { return t.TempDir() }},
			} {
				t.Run(test.name, func(t *testing.T) {
					env, _ := completeConfigEnv(t)
					env[key] = test.makePath(t)
					_, err := LoadConfig(mapGetenv(env))
					assertSafeSettingError(t, err, key)
				})
			}
		})
	}
}

func TestLoadConfigRejectsGroupOrWorldAccessibleSecretFiles(t *testing.T) {
	for _, key := range []string{
		"SHERPA_COLLECTOR_TOKEN_FILE",
		"SHERPA_COLLECTOR_BORG_REPOSITORY_FILE",
		"SHERPA_COLLECTOR_BORG_SSH_KEY_FILE",
	} {
		key := key
		for _, mode := range []os.FileMode{0o640, 0o604, 0o610, 0o601} {
			mode := mode
			t.Run(key+"/"+mode.String(), func(t *testing.T) {
				env, _ := completeConfigEnv(t)
				path := writeTestFile(t, "accessible-secret", "permission-canary", 0o600)
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
				env[key] = path
				_, err := LoadConfig(mapGetenv(env))
				assertSafeSettingError(t, err, key)
			})
		}
	}
}

func TestLoadConfigRejectsMalformedAgeRecipient(t *testing.T) {
	env, _ := completeConfigEnv(t)
	env["SHERPA_COLLECTOR_AGE_RECIPIENT_FILE"] = writeTestFile(t, "recipient", "age1malformed-recipient-canary", 0o644)

	_, err := LoadConfig(mapGetenv(env))
	assertSafeSettingError(t, err, "SHERPA_COLLECTOR_AGE_RECIPIENT_FILE")
}

func TestLoadConfigRejectsAgePrivateIdentity(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	privateCanary := identity.String()
	env, _ := completeConfigEnv(t)
	env["SHERPA_COLLECTOR_AGE_RECIPIENT_FILE"] = writeTestFile(t, "recipient", privateCanary, 0o644)

	_, err = LoadConfig(mapGetenv(env))
	assertSafeSettingError(t, err, "SHERPA_COLLECTOR_AGE_RECIPIENT_FILE")
	if err.Error() != "SHERPA_COLLECTOR_AGE_RECIPIENT_FILE: private identity is not allowed" {
		t.Fatalf("private identity error = %q, want fixed safe classification", err)
	}
	if strings.Contains(err.Error(), privateCanary) || strings.Contains(err.Error(), "AGE-SECRET-KEY") {
		t.Fatalf("error exposed private age identity: %q", err)
	}
}

func TestLoadConfigRejectsInvalidSizesCountsAndDurations(t *testing.T) {
	maxIntOverflow := "9223372036854775808"
	if strconv.IntSize == 32 {
		maxIntOverflow = "2147483648"
	}
	cases := []struct {
		name, key, value string
	}{
		{name: "compressed malformed", key: "SHERPA_COLLECTOR_MAX_BYTES", value: "eight-gibibytes"},
		{name: "compressed zero", key: "SHERPA_COLLECTOR_MAX_BYTES", value: "0"},
		{name: "compressed negative", key: "SHERPA_COLLECTOR_MAX_BYTES", value: "-1"},
		{name: "compressed overflow", key: "SHERPA_COLLECTOR_MAX_BYTES", value: "9223372036854775808"},
		{name: "uncompressed malformed", key: "SHERPA_COLLECTOR_MAX_UNCOMPRESSED_BYTES", value: "large"},
		{name: "uncompressed zero", key: "SHERPA_COLLECTOR_MAX_UNCOMPRESSED_BYTES", value: "0"},
		{name: "uncompressed negative", key: "SHERPA_COLLECTOR_MAX_UNCOMPRESSED_BYTES", value: "-4"},
		{name: "members malformed", key: "SHERPA_COLLECTOR_MAX_MEMBERS", value: "many"},
		{name: "members zero", key: "SHERPA_COLLECTOR_MAX_MEMBERS", value: "0"},
		{name: "members negative", key: "SHERPA_COLLECTOR_MAX_MEMBERS", value: "-7"},
		{name: "members overflow", key: "SHERPA_COLLECTOR_MAX_MEMBERS", value: maxIntOverflow},
		{name: "retry malformed", key: "SHERPA_COLLECTOR_RETRY_INTERVAL", value: "later"},
		{name: "retry zero", key: "SHERPA_COLLECTOR_RETRY_INTERVAL", value: "0s"},
		{name: "retry negative", key: "SHERPA_COLLECTOR_RETRY_INTERVAL", value: "-1s"},
		{name: "retry overflow", key: "SHERPA_COLLECTOR_RETRY_INTERVAL", value: "999999999999h"},
		{name: "partial zero", key: "SHERPA_COLLECTOR_PARTIAL_MAX_AGE", value: "0s"},
		{name: "grace zero", key: "SHERPA_COLLECTOR_STARTUP_GRACE", value: "0s"},
		{name: "recovery zero", key: "SHERPA_COLLECTOR_MAX_RECOVERY_AGE", value: "0s"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			env, _ := completeConfigEnv(t)
			env[test.key] = test.value
			_, err := LoadConfig(mapGetenv(env))
			assertSafeSettingError(t, err, test.key)
		})
	}

	t.Run("recovery age exceeds startup grace", func(t *testing.T) {
		env, _ := completeConfigEnv(t)
		env["SHERPA_COLLECTOR_MAX_RECOVERY_AGE"] = "27h"
		_, err := LoadConfig(mapGetenv(env))
		assertSafeSettingError(t, err, "SHERPA_COLLECTOR_MAX_RECOVERY_AGE")
	})
}

func TestLoadConfigDoesNotInventUnrelatedDurationPolicy(t *testing.T) {
	env, _ := completeConfigEnv(t)
	env["SHERPA_COLLECTOR_PARTIAL_MAX_AGE"] = "27h"

	cfg, err := LoadConfig(mapGetenv(env))
	if err != nil {
		t.Fatalf("LoadConfig rejected an independently valid partial max age: %v", err)
	}
	if cfg.PartialMaxAge != 27*time.Hour {
		t.Fatalf("PartialMaxAge = %v, want 27h", cfg.PartialMaxAge)
	}
}

func TestLoadConfigErrorsDoNotExposeRepositoryTokenOrPrivatePaths(t *testing.T) {
	privatePathCanary := filepath.Join(t.TempDir(), "private-path-canary-77a0")
	if err := os.WriteFile(privatePathCanary, []byte(testSSHKey), 0o644); err != nil {
		t.Fatal(err)
	}
	env, _ := completeConfigEnv(t)
	env["SHERPA_COLLECTOR_BORG_SSH_KEY_FILE"] = privatePathCanary

	_, err := LoadConfig(mapGetenv(env))
	assertSafeSettingError(t, err, "SHERPA_COLLECTOR_BORG_SSH_KEY_FILE")
	for _, canary := range []string{testRepository, testToken, testSSHKey, privatePathCanary, filepath.Dir(privatePathCanary)} {
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("error %q exposed sensitive canary %q", err, canary)
		}
	}

	env, _ = completeConfigEnv(t)
	env["SHERPA_COLLECTOR_BORG_REPOSITORY_FILE"] = writeTestFile(t, "repository", "   "+testRepository+"   ", 0o644)
	_, err = LoadConfig(mapGetenv(env))
	assertSafeSettingError(t, err, "SHERPA_COLLECTOR_BORG_REPOSITORY_FILE")
	if strings.Contains(err.Error(), testRepository) {
		t.Fatalf("error exposed repository value: %q", err)
	}
}

func completeConfigEnv(t testing.TB) (map[string]string, string) {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	recipient := identity.Recipient().String()
	return map[string]string{
		"SHERPA_COLLECTOR_TOKEN_FILE":           writeTestFile(t, "token", "  "+testToken+"\n", 0o600),
		"SHERPA_COLLECTOR_AGE_RECIPIENT_FILE":   writeTestFile(t, "age-recipient", recipient+"\n", 0o644),
		"SHERPA_COLLECTOR_BORG_REPOSITORY_FILE": writeTestFile(t, "borg-repository", "  "+testRepository+"\n", 0o600),
		"SHERPA_COLLECTOR_BORG_SSH_KEY_FILE":    writeTestFile(t, "storage-ssh-key", testSSHKey+"\n", 0o600),
		"SHERPA_COLLECTOR_KNOWN_HOSTS_FILE":     writeTestFile(t, "known-hosts", testKnownHosts+"\n", 0o644),
	}, recipient
}

func writeTestFile(t testing.TB, name, contents string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func mapGetenv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func assertSafeSettingError(t testing.TB, err error, setting string) {
	t.Helper()
	if err == nil {
		t.Fatalf("LoadConfig succeeded, want %s error", setting)
	}
	if !strings.Contains(err.Error(), setting) {
		t.Fatalf("LoadConfig error = %q, want setting %s", err, setting)
	}
}
