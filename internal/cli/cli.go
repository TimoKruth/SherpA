package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sherpa/internal/state"
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
	home, err := filepath.Abs(homeDir())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx := &Ctx{Home: home, Stdout: stdout, Stderr: stderr, Stdin: os.Stdin}
	if args[0] != "serve" && args[0] != "compare" && args[0] != "run" && args[0] != "try" && args[0] != "status" && args[0] != "help" {
		unlock, err := state.Lock(ctx.Home)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer unlock()
	}
	if err := cmd(ctx, args[1:]); err != nil {
		fmt.Fprintf(stderr, "sherpa %s: %v\n", args[0], err)
		return 1
	}
	return 0
}
