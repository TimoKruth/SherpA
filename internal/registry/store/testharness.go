package store

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func StartPostgres(t *testing.T) string {
	t.Helper()

	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker unavailable")
	}

	out, err := exec.Command(
		"docker", "run", "--rm", "-d",
		"-e", "POSTGRES_PASSWORD=pw",
		"-e", "POSTGRES_DB=sherpa",
		"-p", "0:5432",
		"postgres:16",
	).Output()
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	cid := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		_ = exec.Command("docker", "stop", cid).Run()
	})

	portOut, err := exec.Command(
		"docker", "inspect",
		"--format", "{{ (index (index .NetworkSettings.Ports \"5432/tcp\") 0).HostPort }}",
		cid,
	).Output()
	if err != nil {
		t.Fatalf("inspect postgres port: %v", err)
	}
	port := strings.TrimSpace(string(portOut))

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		cmd := exec.Command("docker", "exec", cid, "pg_isready", "-U", "postgres")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil {
			return fmt.Sprintf("postgres://postgres:pw@localhost:%s/sherpa?sslmode=disable", port)
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("postgres did not become ready")
	return ""
}
