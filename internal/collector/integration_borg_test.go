//go:build integration

package collector

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"sherpa/internal/recoveryarchive"
	registryexport "sherpa/internal/registry/export"
)

func TestCollectorEndToEndWithLocalBorgRepository(t *testing.T) {
	binary, err := exec.LookPath("borg")
	if err != nil {
		t.Skip("borg is absent")
	}
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	initCommand := exec.Command(binary, "init", "--encryption=none", repository)
	initCommand.Env = append(nonBorgEnvironment(os.Environ()), "BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes")
	if output, err := initCommand.CombinedOutput(); err != nil {
		t.Fatalf("initialize local Borg repository: %v: %s", err, output)
	}
	workDir := filepath.Join(root, "borg-work")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	backend, err := NewBorgBackend(BorgConfig{
		Binary: binary, Repository: repository,
		SSHKeyFile: filepath.Join(root, "unused-key"), KnownHostsFile: filepath.Join(root, "unused-known-hosts"),
		WorkDir: workDir, CreateTimeout: time.Minute, QueryTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	spool := openTestSpool(t, defaultSpoolOps())
	ledger := openTestLedger(t, defaultLedgerOps())
	now := time.Now().UTC()
	status := NewStatusTracker(ReadinessConfig{StartedAt: now, StartupGrace: time.Hour, MaxRecoveryAge: 24 * time.Hour}, func() time.Time { return now })
	status.SetSpoolWritable(true)
	identity := generateTestIdentity(t)
	service := newService(spool, ledger, NewEncryptor(identity.Recipient()), backend, status, recoveryarchive.DefaultLimits(), serviceOps{
		now: func() time.Time { return now }, validateFile: recoveryarchive.ValidateFile,
	})
	token := "local-borg-upload-token"
	server := httptest.NewServer(integrationHandler(token, service, status))
	defer server.Close()
	archive := validRecoveryArchive(t, "local-borg-http")
	archivePath := filepath.Join(root, "recovery.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := registryexport.Upload(context.Background(), archivePath, server.URL+"/v1/exports", token)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	wantID := "sha256:" + digestHex(archive)
	present, err := backend.Exists(context.Background(), wantID)
	if err != nil || !present || result.ObjectID != wantID || result.Status != "stored" {
		t.Fatalf("result=%#v present=%v err=%v", result, present, err)
	}
}
