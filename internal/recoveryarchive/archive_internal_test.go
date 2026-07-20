package recoveryarchive

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestReadArtifactEnforcesCumulativeLogicalByteLimit(t *testing.T) {
	budget := artifactBudget{limit: 5}
	artifact, size, err := readArtifact(context.Background(), strings.NewReader("abc"), "first", 3, &budget)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Size != 3 || size != 3 || budget.used != 3 {
		t.Fatalf("first copy = (%+v, %d, used %d), want size 3 and used 3", artifact, size, budget.used)
	}

	second := &countingReader{contents: "def"}
	_, _, err = readArtifact(context.Background(), second, "second", 3, &budget)
	if got := Classify(err); got != FailureLimitExceeded {
		t.Fatalf("second copy class = %q, want %q (error: %v)", got, FailureLimitExceeded, err)
	}
	if second.reads != 0 {
		t.Fatalf("over-budget artifact was read %d times", second.reads)
	}
}

func TestReadArtifactChecksContextBetweenReads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &cancelAfterReadReader{cancel: cancel, contents: "ab"}
	budget := artifactBudget{limit: 2}

	_, _, err := readArtifact(ctx, source, "artifact", 2, &budget)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("readArtifact error = %v, want context cancellation", err)
	}
	if source.reads != 1 {
		t.Fatalf("source reads = %d, want 1", source.reads)
	}
}

type countingReader struct {
	contents string
	reads    int
}

func (reader *countingReader) Read(destination []byte) (int, error) {
	reader.reads++
	return strings.NewReader(reader.contents).Read(destination)
}

type cancelAfterReadReader struct {
	cancel   context.CancelFunc
	contents string
	reads    int
}

func (reader *cancelAfterReadReader) Read(destination []byte) (int, error) {
	reader.reads++
	if reader.contents == "" {
		return 0, nil
	}
	destination[0] = reader.contents[0]
	reader.contents = reader.contents[1:]
	reader.cancel()
	return 1, nil
}
