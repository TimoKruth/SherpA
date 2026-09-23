package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sherpa/internal/compare"
	"sherpa/internal/state"
	"strings"
	"time"
)

func init() { register("compare", cmdCompare); register("results", cmdResults) }
func cmdCompare(ctx *Ctx, args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(ctx.Stderr)
	project := fs.String("project", ".", "Git project root")
	prompt := fs.String("prompt", "", "same prompt for all setups")
	promptFile := fs.String("prompt-file", "", "read prompt from a UTF-8 file")
	profiles := fs.String("profiles", "", "comma-separated profile names")
	timeout := fs.Duration("timeout", 5*time.Minute, "timeout per setup (1s–1h)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments; use --profiles and --prompt")
	}
	if *timeout < time.Second || *timeout > time.Hour {
		return fmt.Errorf("timeout must be 1s–1h")
	}
	if *promptFile != "" {
		if *prompt != "" {
			return fmt.Errorf("choose --prompt or --prompt-file")
		}
		b, err := os.ReadFile(*promptFile)
		if err != nil {
			return err
		}
		*prompt = string(b)
	}
	names := strings.Split(*profiles, ",")
	for i := range names {
		names[i] = strings.TrimSpace(names[i])
	}
	unlock, err := state.Lock(ctx.Home)
	if err != nil {
		return err
	}
	c, err := compare.Prepare(ctx.Home, compare.Request{Project: *project, Prompt: *prompt, Profiles: names, TimeoutSeconds: int(timeout.Seconds())})
	unlock()
	if err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "Comparison %s: %d setups, one project snapshot\n", c.ID, len(c.Results))
	runCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := compare.Execute(runCtx, ctx.Home, c); err != nil {
		return err
	}
	for _, r := range c.Results {
		fmt.Fprintf(ctx.Stdout, "%s: %s (%d ms)\n", r.Profile, r.Status, r.DurationMS)
	}
	fmt.Fprintf(ctx.Stdout, "Review: sherpa results %s\nReport: sherpa results %s --format html --output report.html\n", c.ID, c.ID)
	if c.Status != "completed" {
		return fmt.Errorf("comparison %s; saved results remain available", c.Status)
	}
	return nil
}
func cmdResults(ctx *Ctx, args []string) error {
	if len(args) == 0 {
		list, err := compare.List(ctx.Home)
		if err != nil {
			return err
		}
		for _, c := range list {
			fmt.Fprintf(ctx.Stdout, "%s  %s  %s  %d setups\n", c.ID, c.CreatedAt.Format(time.RFC3339), c.Status, len(c.Results))
		}
		return nil
	}
	if args[0] == "rate" {
		if len(args) < 3 {
			return fmt.Errorf("usage: sherpa results rate <id> <profile> --score <1–5> [--notes <text>]")
		}
		fs := flag.NewFlagSet("rate", flag.ContinueOnError)
		fs.SetOutput(ctx.Stderr)
		score := fs.Int("score", 0, "1–5")
		notes := fs.String("notes", "", "private notes")
		if err := fs.Parse(args[3:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected arguments")
		}
		return compare.Rate(ctx.Home, args[1], args[2], *score, *notes)
	}
	fs := flag.NewFlagSet("results", flag.ContinueOnError)
	fs.SetOutput(ctx.Stderr)
	format := fs.String("format", "text", "text, json or html")
	output := fs.String("output", "", "new report file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if *format != "text" && *format != "json" && *format != "html" {
		return fmt.Errorf("format must be text, json or html")
	}
	c, err := compare.Load(ctx.Home, args[0])
	if err != nil {
		return err
	}
	out := ctx.Stdout
	if *output != "" {
		f, err := os.OpenFile(*output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	}
	switch *format {
	case "html":
		return compare.WriteReport(out, c)
	case "json":
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(c)
	default:
		fmt.Fprintf(out, "%s · %s\nPrompt: %s\nSnapshot: %s\n", c.ID, c.Status, terminalLines(c.Request.Prompt, 128<<10), c.ProjectHash)
		for _, r := range c.Results {
			fmt.Fprintf(out, "\n--- %s / %s / %d ms / rating %d ---\n%s\n", r.Profile, r.Status, r.DurationMS, r.Rating, terminalLines(r.Output, 2<<20))
			if r.Error != "" {
				fmt.Fprintf(out, "Error: %s\n", terminalText(r.Error, 4000))
			}
			if r.Diff != "" {
				fmt.Fprintf(out, "Changes:\n%s\n", terminalLines(r.Diff, 2<<20))
			}
			if r.Notes != "" {
				fmt.Fprintf(out, "Notes: %s\n", terminalLines(r.Notes, 16000))
			}
		}
	}
	return nil
}
