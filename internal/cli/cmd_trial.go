package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"sherpa/internal/state"
)

const maxTrialNotesBytes = 4 << 10

type trialRecordRequest struct {
	profile string
	verdict string
	notes   string
}

type trialListRequest struct {
	profile string
}

func init() { register("trial", cmdTrial) }

func cmdTrial(ctx *Ctx, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: sherpa trial record|list|share")
	}
	switch args[0] {
	case "record":
		return cmdTrialRecord(ctx, args[1:])
	case "list":
		return cmdTrialList(ctx, args[1:])
	case "share":
		return cmdTrialShare(ctx, args[1:])
	default:
		return errors.New("usage: sherpa trial record|list|share")
	}
}

func cmdTrialRecord(ctx *Ctx, args []string) error {
	req, err := parseTrialRecordArgs(args)
	if err != nil {
		return err
	}
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	profile, ok := st.Profiles[req.profile]
	if !ok {
		return fmt.Errorf("profile %q not found", req.profile)
	}
	if profile.Registry == nil || isBaselineProfile(st, req.profile) {
		return fmt.Errorf("profile %q is not an installed registry stack", req.profile)
	}
	if profile.Registry.Version < 1 || !validRegistrySegment(profile.Registry.Owner) || !validRegistrySegment(profile.Registry.Stack) {
		return fmt.Errorf("profile %q has invalid registry identity", req.profile)
	}
	id, err := newTrialID(st.Trials)
	if err != nil {
		return errors.New("could not create trial entry ID")
	}
	entry := state.TrialEntry{
		ID: id, Profile: req.profile, RegistryURL: profile.Registry.RegistryURL,
		Owner: profile.Registry.Owner, Stack: profile.Registry.Stack, Version: profile.Registry.Version,
		Verdict: req.verdict, Notes: req.notes, RecordedAt: time.Now().UTC(),
	}
	st.Trials = append(st.Trials, entry)
	if err := st.Save(ctx.Home); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "recorded trial %s for @%s/%s@v%d\n", entry.ID, entry.Owner, entry.Stack, entry.Version)
	return nil
}

func cmdTrialList(ctx *Ctx, args []string) error {
	req, err := parseTrialListArgs(args)
	if err != nil {
		return err
	}
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	entries := make([]state.TrialEntry, 0, len(st.Trials))
	for _, entry := range st.Trials {
		if req.profile == "" || entry.Profile == req.profile {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].RecordedAt.Equal(entries[j].RecordedAt) {
			return entries[i].ID > entries[j].ID
		}
		return entries[i].RecordedAt.After(entries[j].RecordedAt)
	})
	if len(entries) == 0 {
		fmt.Fprintln(ctx.Stdout, "no trial entries")
		return nil
	}
	for _, entry := range entries {
		shared := "local only"
		if entry.SharedAt != nil {
			shared = "shared"
		}
		fmt.Fprintf(ctx.Stdout, "%s  %s  @%s/%s@v%d  %s  %s\n",
			terminalText(entry.ID, 64), terminalText(entry.Profile, 100),
			terminalText(entry.Owner, 100), terminalText(entry.Stack, 100), entry.Version,
			trialVerdictForDisplay(entry.Verdict), shared)
		if notes := terminalText(entry.Notes, maxTrialNotesBytes); notes != "" {
			fmt.Fprintf(ctx.Stdout, "  notes: %s\n", notes)
		}
	}
	return nil
}

func cmdTrialShare(ctx *Ctx, args []string) error {
	if len(args) != 1 || !validTrialID(args[0]) {
		return errors.New("usage: sherpa trial share <entry-id>")
	}
	st, err := state.Load(ctx.Home)
	if err != nil {
		return err
	}
	index := -1
	for i := range st.Trials {
		if st.Trials[i].ID == args[0] {
			index = i
			break
		}
	}
	if index < 0 {
		return errors.New("trial entry not found")
	}
	entry := st.Trials[index]
	if !validTrialVerdict(entry.Verdict) || entry.Version < 1 || !validRegistrySegment(entry.Owner) || !validRegistrySegment(entry.Stack) {
		return errors.New("trial entry has invalid registry data")
	}
	session, err := registryUserSession(ctx.Home, entry.RegistryURL)
	if err != nil {
		return err
	}
	client, err := newRegistrySocialClient(entry.RegistryURL, session.AccessToken)
	if err != nil {
		return err
	}
	if err := client.PutTrial(context.Background(), entry.Owner, entry.Stack, entry.Version, entry.Verdict); err != nil {
		return err
	}
	now := time.Now().UTC()
	st.Trials[index].SharedAt = &now
	if err := st.Save(ctx.Home); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "shared trial %s for @%s/%s@v%d\n", entry.ID, entry.Owner, entry.Stack, entry.Version)
	return nil
}

func parseTrialRecordArgs(args []string) (trialRecordRequest, error) {
	var req trialRecordRequest
	sawVerdict, sawNotes := false, false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--verdict":
			if sawVerdict || i+1 >= len(args) {
				return trialRecordRequest{}, errors.New("usage: sherpa trial record <profile> --verdict keep|keep-with-notes|revert [--notes TEXT]")
			}
			sawVerdict = true
			i++
			req.verdict = normalizeTrialVerdict(args[i])
		case strings.HasPrefix(arg, "--verdict="):
			if sawVerdict {
				return trialRecordRequest{}, errors.New("--verdict may be specified once")
			}
			sawVerdict = true
			req.verdict = normalizeTrialVerdict(strings.TrimPrefix(arg, "--verdict="))
		case arg == "--notes":
			if sawNotes || i+1 >= len(args) {
				return trialRecordRequest{}, errors.New("usage: sherpa trial record <profile> --verdict keep|keep-with-notes|revert [--notes TEXT]")
			}
			sawNotes = true
			i++
			req.notes = args[i]
		case strings.HasPrefix(arg, "--notes="):
			if sawNotes {
				return trialRecordRequest{}, errors.New("--notes may be specified once")
			}
			sawNotes = true
			req.notes = strings.TrimPrefix(arg, "--notes=")
		case strings.HasPrefix(arg, "-"):
			return trialRecordRequest{}, errors.New("unknown trial record flag")
		case req.profile == "":
			req.profile = arg
		default:
			return trialRecordRequest{}, errors.New("usage: sherpa trial record <profile> --verdict keep|keep-with-notes|revert [--notes TEXT]")
		}
	}
	if req.profile == "" || !sawVerdict || !validTrialVerdict(req.verdict) {
		return trialRecordRequest{}, errors.New("usage: sherpa trial record <profile> --verdict keep|keep-with-notes|revert [--notes TEXT]")
	}
	if !utf8.ValidString(req.notes) || len(req.notes) > maxTrialNotesBytes {
		return trialRecordRequest{}, fmt.Errorf("--notes must be valid UTF-8 and at most %d bytes", maxTrialNotesBytes)
	}
	return req, nil
}

func parseTrialListArgs(args []string) (trialListRequest, error) {
	var req trialListRequest
	if len(args) == 0 {
		return req, nil
	}
	if len(args) == 2 && args[0] == "--profile" && args[1] != "" {
		req.profile = args[1]
		return req, nil
	}
	if len(args) == 1 && strings.HasPrefix(args[0], "--profile=") && strings.TrimPrefix(args[0], "--profile=") != "" {
		req.profile = strings.TrimPrefix(args[0], "--profile=")
		return req, nil
	}
	return trialListRequest{}, errors.New("usage: sherpa trial list [--profile NAME]")
}

func newTrialID(entries []state.TrialEntry) (string, error) {
	existing := make(map[string]bool, len(entries))
	for _, entry := range entries {
		existing[entry.ID] = true
	}
	for range 3 {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		id := base64.RawURLEncoding.EncodeToString(raw)
		if !existing[id] {
			return id, nil
		}
	}
	return "", errors.New("trial ID collision")
}

func validTrialID(id string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(id)
	return err == nil && len(decoded) == 16 && base64.RawURLEncoding.EncodeToString(decoded) == id
}

func normalizeTrialVerdict(verdict string) string {
	switch verdict {
	case "keep":
		return "keep"
	case "keep-with-notes":
		return "keep_with_notes"
	case "revert":
		return "revert"
	default:
		return ""
	}
}

func validTrialVerdict(verdict string) bool {
	return verdict == "keep" || verdict == "keep_with_notes" || verdict == "revert"
}

func trialVerdictForDisplay(verdict string) string {
	return strings.ReplaceAll(terminalText(verdict, 32), "_", "-")
}

func isBaselineProfile(st *state.State, name string) bool {
	for _, baseline := range st.Baselines {
		if baseline == name {
			return true
		}
	}
	return name == "mine"
}
