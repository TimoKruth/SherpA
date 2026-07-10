package harness

type Harness interface {
	Name() string
	SetupStateSources(configDir string) []string
	CapturedName() string
	Seed(captured []byte) (targetRel string, content []byte, err error)
}

func Default() Harness { return ClaudeCode{} }
