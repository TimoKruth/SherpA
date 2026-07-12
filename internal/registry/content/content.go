package content

type ContentStore interface {
	EnsureRepo(owner, name string) error
	StageBundle(bundle []byte, gitTag string) (stageDir, worktreeDir string, cleanup func(), err error)
	Commit(owner, name, stageDir string) error
	// RepoPath requires validated owner and name segments. Invalid segments return
	// a path beneath the content root and must not be used for writes.
	RepoPath(owner, name string) string
}
