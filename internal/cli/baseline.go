package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"sherpa/internal/state"
)

func baselineName(st *state.State, harnessName string) (string, bool) {
	n, ok := st.Baselines[harnessName]
	return n, ok
}

// renameProfile moves profiles/old -> profiles/new on disk and updates st in memory.
// It does not Save; the caller commits st and, on Save failure, must roll the dir back.
func renameProfile(home string, st *state.State, old, newName string) error {
	if _, ok := st.Profiles[newName]; ok {
		return fmt.Errorf("profile %q already exists", newName)
	}
	p, ok := st.Profiles[old]
	if !ok {
		return fmt.Errorf("profile %q not found", old)
	}
	oldDir := filepath.Join(home, "profiles", old)
	newDir := filepath.Join(home, "profiles", newName)
	if err := os.Rename(oldDir, newDir); err != nil {
		return err
	}
	delete(st.Profiles, old)
	p.Name = newName
	p.Path = newDir
	st.Profiles[newName] = p
	if st.Active == old {
		st.Active = newName
	}
	for h, name := range st.Baselines {
		if name == old {
			st.Baselines[h] = newName
		}
	}
	return nil
}
