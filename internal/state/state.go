// internal/state/state.go
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"sherpa/internal/registryurl"
)

type RegistryOrigin struct {
	RegistryURL string `json:"registry_url"`
	Owner       string `json:"owner"`
	Stack       string `json:"stack"`
	Version     int    `json:"version"`
}

type Profile struct {
	Name     string          `json:"name"`
	Path     string          `json:"path"`
	Origin   string          `json:"origin"`
	Harness  string          `json:"harness"`
	Registry *RegistryOrigin `json:"registry,omitempty"`
}

type UpdateSummary struct {
	Owner       string    `json:"owner"`
	Stack       string    `json:"stack"`
	Version     int       `json:"version"`
	SeenVersion int       `json:"seen_version"`
	Changelog   string    `json:"changelog,omitempty"`
	PublishedAt time.Time `json:"published_at,omitempty"`
}

type RegistryState struct {
	PendingFollows []string        `json:"pending_follows,omitempty"`
	CachedUpdates  []UpdateSummary `json:"cached_updates,omitempty"`
	LastCheckedAt  time.Time       `json:"last_checked_at,omitempty"`
}

type TrialEntry struct {
	ID          string     `json:"id"`
	Profile     string     `json:"profile"`
	RegistryURL string     `json:"registry_url"`
	Owner       string     `json:"owner"`
	Stack       string     `json:"stack"`
	Version     int        `json:"version"`
	Verdict     string     `json:"verdict"`
	Notes       string     `json:"notes,omitempty"`
	RecordedAt  time.Time  `json:"recorded_at"`
	SharedAt    *time.Time `json:"shared_at,omitempty"`
}

type State struct {
	Active     string                   `json:"active"`
	Profiles   map[string]Profile       `json:"profiles"`
	Baselines  map[string]string        `json:"baselines"`
	Registries map[string]RegistryState `json:"registries,omitempty"`
	Trials     []TrialEntry             `json:"trials,omitempty"`
}

func file(home string) string { return filepath.Join(home, "state.json") }

func Load(home string) (*State, error) {
	s := &State{Profiles: map[string]Profile{}, Baselines: map[string]string{}, Registries: map[string]RegistryState{}}
	b, err := os.ReadFile(file(home))
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	if s.Profiles == nil {
		s.Profiles = map[string]Profile{}
	}
	if s.Baselines == nil {
		s.Baselines = map[string]string{}
	}
	if s.Registries == nil {
		s.Registries = map[string]RegistryState{}
	}
	if err := s.normalizeRegistryState(); err != nil {
		return nil, err
	}
	if len(s.Baselines) == 0 {
		if m, ok := s.Profiles["mine"]; ok {
			s.Baselines[m.Harness] = "mine"
		}
	}
	return s, nil
}

func (s *State) Save(home string) error {
	if err := s.normalizeRegistryState(); err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := file(home) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, file(home))
}

func (s *State) normalizeRegistryState() error {
	normalized := make(map[string]RegistryState, len(s.Registries))
	keys := make([]string, 0, len(s.Registries))
	for key := range s.Registries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		canonical, err := registryurl.Normalize(key)
		if err != nil {
			return fmt.Errorf("registry state key %q: %w", key, err)
		}
		current := normalized[canonical]
		incoming := s.Registries[key]
		current.PendingFollows = mergeStrings(current.PendingFollows, incoming.PendingFollows)
		current.CachedUpdates = mergeUpdates(current.CachedUpdates, incoming.CachedUpdates)
		if incoming.LastCheckedAt.After(current.LastCheckedAt) {
			current.LastCheckedAt = incoming.LastCheckedAt
		}
		normalized[canonical] = current
	}
	s.Registries = normalized
	for name, profile := range s.Profiles {
		if profile.Registry == nil {
			continue
		}
		canonical, err := registryurl.Normalize(profile.Registry.RegistryURL)
		if err != nil {
			return fmt.Errorf("profile %q registry: %w", name, err)
		}
		profile.Registry.RegistryURL = canonical
		s.Profiles[name] = profile
	}
	for i := range s.Trials {
		if s.Trials[i].RegistryURL == "" {
			continue
		}
		canonical, err := registryurl.Normalize(s.Trials[i].RegistryURL)
		if err != nil {
			return fmt.Errorf("trial %q registry: %w", s.Trials[i].ID, err)
		}
		s.Trials[i].RegistryURL = canonical
	}
	return nil
}

func mergeStrings(left, right []string) []string {
	seen := make(map[string]bool, len(left)+len(right))
	merged := make([]string, 0, len(left)+len(right))
	for _, value := range append(append([]string{}, left...), right...) {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		merged = append(merged, value)
	}
	sort.Strings(merged)
	return merged
}

func mergeUpdates(left, right []UpdateSummary) []UpdateSummary {
	byKey := make(map[string]UpdateSummary, len(left)+len(right))
	for _, update := range append(append([]UpdateSummary{}, left...), right...) {
		key := update.Owner + "/" + update.Stack
		if current, ok := byKey[key]; !ok || update.Version > current.Version {
			byKey[key] = update
		}
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	merged := make([]UpdateSummary, 0, len(keys))
	for _, key := range keys {
		merged = append(merged, byKey[key])
	}
	return merged
}
