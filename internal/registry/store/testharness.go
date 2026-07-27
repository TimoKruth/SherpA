package store

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
	dsn := fmt.Sprintf("postgres://postgres:pw@localhost:%s/sherpa?sslmode=disable", port)

	// The postgres image runs a temporary server on the unix socket while it
	// initializes, then stops it and starts the real one. `pg_isready` against
	// that socket reports ready during the temporary phase, so a caller can
	// connect into the shutdown and get "connection reset by peer". Probing
	// over TCP avoids this: the temporary server sets listen_addresses='' and
	// never accepts a TCP connection. Two consecutive successful round trips
	// guard against connecting to a server that is about to be replaced.
	deadline := time.Now().Add(90 * time.Second)
	streak := 0
	var lastErr error
	for time.Now().Before(deadline) {
		if err := pingPostgres(dsn); err != nil {
			lastErr = err
			streak = 0
			time.Sleep(500 * time.Millisecond)
			continue
		}
		streak++
		if streak >= 2 {
			return dsn
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("postgres did not become ready: %v", lastErr)
	return ""
}

func pingPostgres(dsn string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	var one int
	if err := conn.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		return err
	}
	return nil
}
