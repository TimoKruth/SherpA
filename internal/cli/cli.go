package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Version is overridden at release time with -ldflags "-X sherpa/internal/cli.Version=<tag>".
var Version = "0.1.0-dev"

// DefaultRegistryURL is the registry used when SHERPA_REGISTRY_URL is unset, so
// a fresh install can search and log in without configuration. Overridden at
// build time with -ldflags "-X sherpa/internal/cli.DefaultRegistryURL=<url>".
var DefaultRegistryURL = "https://registry.trysherpa.net"

// registryBaseURL returns the configured registry, falling back to the build
// default. An empty environment value counts as unset.
func registryBaseURL() string {
	if configured := strings.TrimSpace(os.Getenv("SHERPA_REGISTRY_URL")); configured != "" {
		return configured
	}
	return DefaultRegistryURL
}

type Ctx struct {
	Home   string
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
}

type command func(ctx *Ctx, args []string) error

var commands = map[string]command{}

func register(name string, fn command) { commands[name] = fn }

func homeDir() string {
	if h := os.Getenv("SHERPA_HOME"); h != "" {
		return h
	}
	u, _ := os.UserHomeDir()
	return filepath.Join(u, ".sherpa")
}

func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		writeHelp(stderr)
		return 2
	}
	if args[0] == "version" {
		fmt.Fprintf(stdout, "sherpa %s\n", Version)
		return 0
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "sherpa: unknown command %q\n\n", args[0])
		writeHelp(stderr)
		return 2
	}
	ctx := &Ctx{Home: homeDir(), Stdout: stdout, Stderr: stderr, Stdin: os.Stdin}
	if err := cmd(ctx, args[1:]); err != nil {
		fmt.Fprintf(stderr, "sherpa %s: %v\n", args[0], err)
		return 1
	}
	return 0
}
