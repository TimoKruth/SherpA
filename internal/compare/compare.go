// Package compare runs independent local trials and persists private results.
package compare

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sherpa/internal/harness"
	"sherpa/internal/launch"
	"sherpa/internal/profile"
	"sherpa/internal/state"
	"sort"
	"strings"
	"time"
)

type Request struct {
	Project        string   `json:"project"`
	Prompt         string   `json:"prompt"`
	Profiles       []string `json:"profiles"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}
type Result struct {
	Profile     string `json:"profile"`
	Harness     string `json:"harness"`
	ConfigHash  string `json:"config_hash"`
	ToolVersion string `json:"tool_version"`
	Status      string `json:"status"`
	DurationMS  int64  `json:"duration_ms"`
	ExitCode    int    `json:"exit_code"`
	Output      string `json:"output"`
	Stderr      string `json:"stderr"`
	Diff        string `json:"diff"`
	Error       string `json:"error,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
	Rating      int    `json:"rating,omitempty"`
	Notes       string `json:"notes,omitempty"`
}
type Comparison struct {
	ID          string    `json:"id"`
	CreatedAt   time.Time `json:"created_at"`
	Status      string    `json:"status"`
	Request     Request   `json:"request"`
	ProjectHash string    `json:"project_hash"`
	Revision    string    `json:"revision"`
	Results     []Result  `json:"results"`
}

var idPattern = regexp.MustCompile(`^[a-f0-9]{24}$`)

func directory(home, id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("invalid comparison ID")
	}
	return filepath.Join(home, "comparisons", id), nil
}

// Prepare freezes all configurations and the project before the first run.
func Prepare(home string, req Request) (*Comparison, error) {
	if strings.TrimSpace(req.Project) == "" {
		return nil, fmt.Errorf("choose a project directory")
	}
	if strings.TrimSpace(req.Prompt) == "" || len(req.Prompt) > 128<<10 {
		return nil, fmt.Errorf("prompt must contain 1–131072 bytes")
	}
	if len(req.Profiles) < 2 || len(req.Profiles) > 8 {
		return nil, fmt.Errorf("select 2–8 distinct profiles")
	}
	if req.TimeoutSeconds == 0 {
		req.TimeoutSeconds = 300
	}
	if req.TimeoutSeconds < 1 || req.TimeoutSeconds > 3600 {
		return nil, fmt.Errorf("timeout must be 1–3600 seconds per setup")
	}
	source, err := filepath.Abs(req.Project)
	if err != nil {
		return nil, err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return nil, err
	}
	req.Project = source
	absHome, err := filepath.Abs(home)
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(absHome); err == nil {
		absHome = resolved
	}
	if absHome == source || strings.HasPrefix(absHome, source+string(os.PathSeparator)) {
		return nil, fmt.Errorf("SHERPA_HOME must be outside the project")
	}
	st, err := state.Load(home)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, name := range req.Profiles {
		if seen[name] {
			return nil, fmt.Errorf("duplicate profile %q", name)
		}
		seen[name] = true
		p, ok := st.Profiles[name]
		if !ok {
			return nil, fmt.Errorf("unknown profile %q", name)
		}
		if _, ok := st.Profiles[st.Baselines[p.Harness]]; !ok {
			return nil, fmt.Errorf("missing baseline for %s", p.Harness)
		}
		if _, err := harness.For(p.Harness); err != nil {
			return nil, err
		}
	}
	token := make([]byte, 12)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	c := &Comparison{ID: hex.EncodeToString(token), CreatedAt: time.Now().UTC(), Status: "ready", Request: req, Results: []Result{}}
	dir, _ := directory(home, c.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(dir)
		}
	}()
	c.ProjectHash, c.Revision, err = snapshot(source, filepath.Join(dir, "snapshot"))
	if err != nil {
		return nil, err
	}
	for i, name := range req.Profiles {
		p := st.Profiles[name]
		h, _ := harness.For(p.Harness)
		base := st.Profiles[st.Baselines[p.Harness]]
		setup := filepath.Join(dir, fmt.Sprintf("run-%d", i), "setup")
		config := filepath.Join(dir, fmt.Sprintf("run-%d", i), "config")
		if err := profile.CopyConfig(p.Path, setup, h, false); err != nil {
			return nil, err
		}
		if err := profile.CopyConfig(setup, config, h, false); err != nil {
			return nil, err
		}
		digest, err := profile.Digest(config, h)
		if err != nil {
			return nil, err
		}
		// Profile-specific identity wins; baseline identity is a fallback. Nothing
		// is linked and neither source receives runtime writes.
		if err := launch.CopyCredentials(h, config, p.Path); err != nil {
			return nil, err
		}
		if err := launch.CopyCredentials(h, config, base.Path); err != nil {
			return nil, err
		}
		if err := launch.SeedSetup(config, base.Path, h); err != nil {
			return nil, err
		}
		c.Results = append(c.Results, Result{Profile: name, Harness: p.Harness, ConfigHash: digest, Status: "queued", ExitCode: -1})
	}
	if err := Save(home, c); err != nil {
		return nil, err
	}
	success = true
	return c, nil
}

// Execute persists after each transition. Failed trials do not suppress later
// trials. Cancellation preserves partial results and skips remaining setups.
func Execute(ctx context.Context, home string, c *Comparison) error {
	dir, err := directory(home, c.ID)
	if err != nil {
		return err
	}
	defer func() {
		for i := range c.Results {
			os.RemoveAll(filepath.Join(dir, fmt.Sprintf("run-%d", i), "config"))
		}
	}()
	c.Status = "running"
	if err := Save(home, c); err != nil {
		return err
	}
	for i := range c.Results {
		r := &c.Results[i]
		if ctx.Err() != nil {
			r.Status = "cancelled"
			r.Error = "comparison cancelled"
			continue
		}
		r.Status = "running"
		if err := Save(home, c); err != nil {
			return err
		}
		runCtx, cancel := context.WithTimeout(ctx, time.Duration(c.Request.TimeoutSeconds)*time.Second)
		executeOne(runCtx, dir, i, c.Request.Prompt, r)
		cancel()
		if err := Save(home, c); err != nil {
			return err
		}
	}
	c.Status = "completed"
	for _, r := range c.Results {
		if r.Status != "completed" {
			c.Status = "completed_with_errors"
		}
	}
	if ctx.Err() != nil {
		c.Status = "cancelled"
	}
	// Trial credentials and runtime data are never retained in reports or runs.
	for i := range c.Results {
		if err := os.RemoveAll(filepath.Join(dir, fmt.Sprintf("run-%d", i), "config")); err != nil {
			return fmt.Errorf("remove trial credentials: %w", err)
		}
	}
	return Save(home, c)
}
func executeOne(ctx context.Context, dir string, i int, prompt string, r *Result) {
	start := time.Now()
	defer func() { r.DurationMS = time.Since(start).Milliseconds() }()
	r.Status = "failed"
	run := filepath.Join(dir, fmt.Sprintf("run-%d", i))
	work := filepath.Join(run, "project")
	config := filepath.Join(run, "config")
	if err := copyProject(filepath.Join(dir, "snapshot"), work); err != nil {
		r.Error = err.Error()
		return
	}
	if err := initProject(work); err != nil {
		r.Error = err.Error()
		return
	}
	baseCommit, err := git(work, "rev-parse", "HEAD")
	if err != nil {
		r.Error = err.Error()
		return
	}
	h, _ := harness.For(r.Harness)
	if err := h.PrepareBaselineCredentials(config); err != nil {
		r.Stderr = "Credential preparation: " + err.Error() + "\n"
	}
	versionCtx, versionCancel := context.WithTimeout(ctx, 5*time.Second)
	version := launch.Command(versionCtx, h, config, work, []string{"--version"})
	var vb cappedBuffer
	version.Stdout = &vb
	version.Stderr = &vb
	if version.Run() == nil {
		r.ToolVersion = strings.TrimSpace(vb.String())
	}
	versionCancel()
	var args []string
	// Claude noninteractive permissions stay in its normal mode; Codex is
	// constrained to the trial workspace. Never pass permission bypass flags.
	if h.Name() == "claude-code" {
		args = []string{"--print", "--output-format", "text", "--permission-mode", "dontAsk"}
	} else {
		args = []string{"exec", "--sandbox", "workspace-write", "--color", "never", "-"}
	}
	cmd := launch.Command(ctx, h, config, work, args)
	cmd.Stdin = strings.NewReader(prompt)
	var out, errout cappedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &errout
	err = cmd.Run()
	r.Output = out.String()
	r.Stderr += errout.String()
	r.Truncated = out.truncated || errout.truncated
	if err == nil {
		r.ExitCode = 0
		r.Status = "completed"
	} else {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			r.ExitCode = exit.ExitCode()
		}
		r.Error = err.Error()
		if ctx.Err() != nil {
			r.Status = "cancelled"
			r.Error = ctx.Err().Error()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				r.Status = "timed_out"
			}
		}
	}
	// Capture additions, modifications and deletions against the frozen start.
	if _, err := git(work, "add", "--all", "--", "."); err != nil {
		r.Error += "; capture changes: " + err.Error()
		r.Status = "failed"
		return
	}
	diff, err := git(work, "diff", "--cached", "--no-ext-diff", "--no-textconv", "--no-color", baseCommit, "--")
	if err != nil {
		r.Error += "; diff: " + err.Error()
		r.Status = "failed"
		return
	}
	if len(diff) > maxOutput {
		diff = diff[:maxOutput] + "\n[diff truncated]"
		r.Truncated = true
	}
	r.Diff = diff
}

const maxOutput = 2 << 20

type cappedBuffer struct {
	b         []byte
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxOutput - len(b.b)
	if n > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	b.b = append(b.b, p...)
	return n, nil
}
func (b *cappedBuffer) String() string { return string(b.b) }
func Save(home string, c *Comparison) error {
	dir, err := directory(home, c.ID)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".result-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "result.json"))
}
func Load(home, id string) (*Comparison, error) {
	dir, err := directory(home, id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, "result.json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var c Comparison
	if err := json.NewDecoder(io.LimitReader(f, 64<<20)).Decode(&c); err != nil {
		return nil, err
	}
	return &c, nil
}
func List(home string) ([]Comparison, error) {
	entries, err := os.ReadDir(filepath.Join(home, "comparisons"))
	if os.IsNotExist(err) {
		return []Comparison{}, nil
	}
	if err != nil {
		return nil, err
	}
	all := []Comparison{}
	for _, e := range entries {
		if !e.IsDir() || !idPattern.MatchString(e.Name()) {
			continue
		}
		c, err := Load(home, e.Name())
		if err != nil {
			return nil, err
		}
		for i := range c.Results {
			c.Results[i].Output = ""
			c.Results[i].Stderr = ""
			c.Results[i].Diff = ""
		}
		all = append(all, *c)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	return all, nil
}
func Rate(home, id, name string, rating int, notes string) error {
	if rating < 1 || rating > 5 || len(notes) > 16000 {
		return fmt.Errorf("rating must be 1–5; notes at most 16000 bytes")
	}
	c, err := Load(home, id)
	if err != nil {
		return err
	}
	if c.Status == "ready" || c.Status == "running" {
		return fmt.Errorf("wait for the comparison to finish before rating")
	}
	for i := range c.Results {
		if c.Results[i].Profile == name {
			c.Results[i].Rating = rating
			c.Results[i].Notes = notes
			return Save(home, c)
		}
	}
	return fmt.Errorf("profile not present in comparison")
}
