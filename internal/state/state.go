// internal/state/state.go
package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

type Profile struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Origin  string `json:"origin"`
	Harness string `json:"harness"`
}

type State struct {
	Active   string             `json:"active"`
	Profiles map[string]Profile `json:"profiles"`
}

func file(home string) string { return filepath.Join(home, "state.json") }

func Load(home string) (*State, error) {
	s := &State{Profiles: map[string]Profile{}}
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
	tmp := file(home) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file(home))
}
