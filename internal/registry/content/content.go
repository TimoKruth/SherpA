package content

type ContentStore interface {
	EnsureRepo(owner, name string) error
	StageBundle(bundle []byte, gitTag string) (stageDir, worktreeDir string, err error)
	Commit(owner, name, stageDir string) error
	RepoPath(owner, name string) string
}
