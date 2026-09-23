// internal/state/state.go
package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Legacy registry metadata is retained opaquely when existing installations are
// opened. V1 never reads it, contacts an issuer, or discards private trial notes.
type Profile struct {
	Name     string          `json:"name"`
	Path     string          `json:"path"`
	Origin   string          `json:"origin"`
	Harness  string          `json:"harness"`
	Registry json.RawMessage `json:"registry,omitempty"`
}
type State struct {
	Active     string             `json:"active"`
	Profiles   map[string]Profile `json:"profiles"`
	Baselines  map[string]string  `json:"baselines"`
	Registries json.RawMessage    `json:"registries,omitempty"`
	Trials     json.RawMessage    `json:"trials,omitempty"`
}

func file(home string) string { return filepath.Join(home, "state.json") }

func Load(home string) (*State, error) {
	s := &State{Profiles: map[string]Profile{}, Baselines: map[string]string{}}
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
	if len(s.Baselines) == 0 {
		if m, ok := s.Profiles["mine"]; ok {
			s.Baselines[m.Harness] = "mine"
		}
	}
	return s, nil
}

func (s *State) Save(home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(home, ".state-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
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
