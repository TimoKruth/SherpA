package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sherpa/internal/harness"
	"sherpa/internal/profile"
	"sherpa/internal/state"
)

func init() { register("init", cmdInit) }

func configDir(h harness.Harness) string {
	envKey := "SHERPA_" + strings.ToUpper(h.Alias()) + "_DIR"
	if d := os.Getenv(envKey); d != "" {
		return d
	}
	u, _ := os.UserHomeDir()
	return h.DefaultConfigDir(u)
}

type detectedSetup struct {
	harness harness.Harness
	source  string
}

type plannedBaseline struct {
	setup detectedSetup
	name  string
	dest  string
}

func cmdInit(ctx *Ctx, args []string) error {
	refresh := false
	harnessName := ""
	primaryName := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--refresh":
			refresh = true
		case "--harness":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" {
				return fmt.Errorf("--harness requires a name")
			}
			harnessName = strings.TrimSpace(args[i])
		case "--primary-harness":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" {
				return fmt.Errorf("--primary-harness requires a name")
			}
			primaryName = strings.TrimSpace(args[i])
		default:
			return fmt.Errorf("unknown argument %q", args[i])
		}
	}
	if harnessName != "" && primaryName != "" {
		return errors.New("--harness and --primary-harness cannot be used together")
	}
	if refresh && primaryName != "" {
		return errors.New("--primary-harness cannot be used with --refresh")
	}

	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	if refresh {
		return refreshBaseline(ctx, st, harnessName)
	}

	detected, err := detectHarnessSetups()
	if err != nil {
		return err
	}
	if len(st.Baselines) == 0 {
		if len(detected) == 0 {
			return fmt.Errorf("no supported harness setup detected (checked: %s)", strings.Join(detectedSetupPaths(), ", "))
		}
		selected := primaryName
		if selected == "" {
			selected = harnessName
		}
		if selected == "" {
			selected, err = confirmInitialSetups(ctx, detected)
			if err != nil {
				return err
			}
		} else {
			selected, err = detectedHarnessName(selected, detected)
			if err != nil {
				return err
			}
		}
		return importDetectedSetups(ctx, st, detected, selected)
	}

	if primaryName != "" {
		return errors.New("a primary baseline is already established; --primary-harness is only valid for first initialization")
	}
	if harnessName != "" {
		h, err := resolveHarness(harnessName)
		if err != nil {
			return err
		}
		if baseline, ok := baselineName(st, h.Name()); ok {
			fmt.Fprintf(ctx.Stdout, "%s is already initialized as profile %q; use --refresh to re-capture setup state\n", h.Name(), baseline)
			return nil
		}
		setup, err := detectHarnessSetup(h)
		if err != nil {
			return err
		}
		return importDetectedSetups(ctx, st, []detectedSetup{setup}, "")
	}

	newSetups := make([]detectedSetup, 0, len(detected))
	for _, setup := range detected {
		if _, exists := baselineName(st, setup.harness.Name()); !exists {
			newSetups = append(newSetups, setup)
		}
	}
	if len(newSetups) == 0 {
		fmt.Fprintln(ctx.Stdout, "all detected harness setups are already initialized")
		return nil
	}
	printDetectedSetups(ctx.Stdout, newSetups)
	if err := confirm(bufio.NewReader(ctx.Stdin), ctx.Stdout,
		"Import the newly detected setups as protected harness baselines? Type yes to confirm: "); err != nil {
		return err
	}
	return importDetectedSetups(ctx, st, newSetups, "")
}

func detectHarnessSetups() ([]detectedSetup, error) {
	setups := make([]detectedSetup, 0, len(harness.Names()))
	for _, name := range harness.Names() {
		h, err := harness.For(name)
		if err != nil {
			return nil, err
		}
		path := configDir(h)
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect %s setup at %s: %w", h.Name(), path, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s setup path is not a directory: %s", h.Name(), path)
		}
		setups = append(setups, detectedSetup{harness: h, source: path})
	}
	return setups, nil
}

func detectHarnessSetup(h harness.Harness) (detectedSetup, error) {
	path := configDir(h)
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return detectedSetup{}, fmt.Errorf("%s setup directory not found: %s", h.Name(), path)
	}
	if err != nil {
		return detectedSetup{}, err
	}
	if !info.IsDir() {
		return detectedSetup{}, fmt.Errorf("%s setup path is not a directory: %s", h.Name(), path)
	}
	return detectedSetup{harness: h, source: path}, nil
}

func detectedSetupPaths() []string {
	paths := make([]string, 0, len(harness.Names()))
	for _, name := range harness.Names() {
		h, err := harness.For(name)
		if err == nil {
			paths = append(paths, h.Name()+"="+configDir(h))
		}
	}
	return paths
}

func printDetectedSetups(out io.Writer, setups []detectedSetup) {
	fmt.Fprintln(out, "Detected harness setups:")
	for _, setup := range setups {
		fmt.Fprintf(out, "  %s: %s\n", setup.harness.Name(), setup.source)
	}
}

func confirmInitialSetups(ctx *Ctx, setups []detectedSetup) (string, error) {
	printDetectedSetups(ctx.Stdout, setups)
	reader := bufio.NewReader(ctx.Stdin)
	if len(setups) == 1 {
		name := setups[0].harness.Name()
		if err := confirm(reader, ctx.Stdout,
			fmt.Sprintf("Import %s as the canonical protected profile \"mine\"? Type yes to confirm: ", name)); err != nil {
			return "", err
		}
		return name, nil
	}

	if defaultName := harness.Default().Name(); setupDetected(defaultName, setups) {
		fmt.Fprintf(ctx.Stdout, "Proposed primary: %s\n", defaultName)
	}
	fmt.Fprint(ctx.Stdout, "Which detected harness should own the canonical profile \"mine\"? Enter its name: ")
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	name, err := detectedHarnessName(strings.TrimSpace(line), setups)
	if err != nil {
		return "", err
	}
	if err := confirm(reader, ctx.Stdout,
		fmt.Sprintf("Import %s as \"mine\" and all other detected setups as optional protected baselines? Type yes to confirm: ", name)); err != nil {
		return "", err
	}
	return name, nil
}

func setupDetected(name string, setups []detectedSetup) bool {
	for _, setup := range setups {
		if setup.harness.Name() == name {
			return true
		}
	}
	return false
}

func detectedHarnessName(input string, setups []detectedSetup) (string, error) {
	if input == "" {
		return "", errors.New("a detected primary harness must be selected explicitly")
	}
	for _, setup := range setups {
		if input == setup.harness.Name() || input == setup.harness.Alias() {
			return setup.harness.Name(), nil
		}
	}
	return "", fmt.Errorf("harness %q was not detected", input)
}

func resolveHarness(input string) (harness.Harness, error) {
	if h, err := harness.For(input); err == nil {
		return h, nil
	}
	for _, name := range harness.Names() {
		h, err := harness.For(name)
		if err == nil && h.Alias() == input {
			return h, nil
		}
	}
	return nil, fmt.Errorf("unknown harness %q (known: %v)", input, harness.Names())
}

func importDetectedSetups(ctx *Ctx, st *state.State, setups []detectedSetup, primaryName string) error {
	plans := make([]plannedBaseline, 0, len(setups))
	for _, setup := range setups {
		name := "mine-" + setup.harness.Alias()
		if primaryName != "" && setup.harness.Name() == primaryName {
			name = "mine"
		}
		if _, exists := st.Profiles[name]; exists {
			return fmt.Errorf("profile %q already exists in state", name)
		}
		dest := filepath.Join(ctx.Home, "profiles", name)
		if err := ensureProfileDirAbsent(dest); err != nil {
			return err
		}
		plans = append(plans, plannedBaseline{setup: setup, name: name, dest: dest})
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].name < plans[j].name })

	created := make([]string, 0, len(plans))
	cleanup := func() {
		for _, dest := range created {
			_ = os.RemoveAll(dest)
		}
	}
	for _, plan := range plans {
		created = append(created, plan.dest)
		if err := profile.Import(plan.setup.source, plan.dest, plan.setup.harness.GitignoreContent()); err != nil {
			cleanup()
			return err
		}
		if err := captureSetupState(plan.dest, plan.setup.harness); err != nil {
			cleanup()
			return err
		}
	}

	for _, plan := range plans {
		st.Profiles[plan.name] = state.Profile{Name: plan.name, Path: plan.dest, Harness: plan.setup.harness.Name()}
		st.Baselines[plan.setup.harness.Name()] = plan.name
		if plan.name == "mine" && st.Active == "" {
			st.Active = plan.name
		}
	}
	if err := st.Save(ctx.Home); err != nil {
		cleanup()
		return err
	}
	for _, plan := range plans {
		fmt.Fprintf(ctx.Stdout, "imported %s as profile %q (your original config is untouched)\n", plan.setup.source, plan.name)
	}
	return nil
}

func refreshBaseline(ctx *Ctx, st *state.State, requested string) error {
	name := requested
	if name == "" {
		switch len(st.Baselines) {
		case 0:
			return fmt.Errorf("nothing to refresh: run `sherpa init` first")
		case 1:
			for existing := range st.Baselines {
				name = existing
			}
		default:
			return errors.New("multiple harness baselines exist; use --harness with --refresh")
		}
	}
	h, err := resolveHarness(name)
	if err != nil {
		return err
	}
	baseline, exists := baselineName(st, h.Name())
	if !exists {
		return fmt.Errorf("nothing to refresh for %s: run `sherpa init --harness %s` first", h.Name(), h.Name())
	}
	dest := filepath.Join(ctx.Home, "profiles", baseline)
	if p, ok := st.Profiles[baseline]; ok && p.Path != "" {
		dest = p.Path
	}
	if err := captureSetupState(dest, h); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "refreshed setup state for profile %q\n", baseline)
	return nil
}

func ensureProfileDirAbsent(dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("profile directory already exists: %s (refusing to overwrite)", dest)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// captureSetupState copies each existing harness setup-state source into the
// baseline as the untracked captured blob (0600). Missing source is not an error.
func captureSetupState(mineDir string, h harness.Harness) error {
	for _, src := range h.SetupStateSources(configDir(h)) {
		b, err := os.ReadFile(src)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(mineDir, h.CapturedName()), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}
