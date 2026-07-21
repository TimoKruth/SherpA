//go:build integration

package collector

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBorgLocalRepositoryCreateAndExactPresence(t *testing.T) {
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

	objectDir := filepath.Join(root, "objects")
	if err := os.Mkdir(objectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.Join(objectDir, testDigestHex+".tar.gz.age")
	if err := os.WriteFile(objectPath, []byte("local-integration-encrypted-object"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend, err := NewBorgBackend(BorgConfig{
		Binary:         binary,
		Repository:     repository,
		SSHKeyFile:     filepath.Join(root, "unused-key"),
		KnownHostsFile: filepath.Join(root, "unused-known-hosts"),
		WorkDir:        filepath.Join(root, "work"),
		CreateTimeout:  time.Minute,
		QueryTimeout:   time.Minute,
	})
	if err != nil {
		t.Fatalf("NewBorgBackend: %v", err)
	}
	object := PendingObject{
		ObjectID:      "sha256:" + testDigestHex,
		DigestHex:     testDigestHex,
		ArchiveName:   "sherpa-" + testDigestHex,
		EncryptedPath: objectPath,
		EncryptedSize: 34,
		ReceivedAt:    time.Now(),
	}
	if err := backend.Create(context.Background(), object); err != nil {
		t.Fatalf("Create: %v", err)
	}
	exists, err := backend.Exists(context.Background(), object.ObjectID)
	if err != nil || !exists {
		t.Fatalf("Exists = %v, %v", exists, err)
	}
	exists, err = backend.Exists(context.Background(), "sha256:"+strings.Repeat("f", 64))
	if err != nil || exists {
		t.Fatalf("unrelated Exists = %v, %v", exists, err)
	}
}
