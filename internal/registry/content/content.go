package content

import "errors"

var ErrNotFound = errors.New("content not found")

type RepositoryRef struct {
	Owner string
	Name  string
}

type ContentStore interface {
	EnsureRepo(owner, name string) error
	StageBundle(bundle []byte, gitTag string) (stageDir, worktreeDir string, cleanup func(), err error)
	Commit(owner, name, stageDir string) error
	TagCommit(owner, name, tag string) (commit string, err error)
	ListRepositories() ([]RepositoryRef, error)
	ListTags(owner, name string) ([]string, error)
	// RepoPath requires validated owner and name segments. Invalid segments return
	// a path beneath the content root and must not be used for writes.
	RepoPath(owner, name string) string
}
