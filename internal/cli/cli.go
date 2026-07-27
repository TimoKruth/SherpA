package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Version is overridden at release time with -ldflags "-X sherpa/internal/cli.Version=<tag>".
var Version = "0.1.0-dev"

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
		fmt.Fprintln(stderr, "usage: sherpa <command> [args]")
		return 2
	}
	if args[0] == "version" {
		fmt.Fprintf(stdout, "sherpa %s\n", Version)
		return 0
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "sherpa: unknown command %q\n", args[0])
		return 2
	}
	ctx := &Ctx{Home: homeDir(), Stdout: stdout, Stderr: stderr, Stdin: os.Stdin}
	if err := cmd(ctx, args[1:]); err != nil {
		fmt.Fprintf(stderr, "sherpa %s: %v\n", args[0], err)
		return 1
	}
	return 0
}
