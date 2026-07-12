package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type Stack struct {
	ID         int64
	Owner      string
	Name       string
	Summary    string
	Harness    string
	ForkedFrom string
	Tags       []string
	CreatedAt  time.Time
}

type Version struct {
	ID          int64
	StackID     int64
	Version     int
	GitTag      string
	Manifest    json.RawMessage
	ScanReport  json.RawMessage
	Changelog   string
	TrustTier   string
	PublishedAt time.Time
}

type StackWithLatest struct {
	Stack
	Version     int
	GitTag      string
	TrustTier   string
	PublishedAt time.Time
}

// VersionRef is the durable identity of a version's Git tag.
type VersionRef struct {
	Owner   string
	Name    string
	Version int
	GitTag  string
}

type Store interface {
	UpsertUser(ctx context.Context, handle string) (userID int64, err error)
	UpsertUserGitHub(ctx context.Context, login string, githubID int64) (userID int64, err error)
	CreateSession(ctx context.Context, userID int64, tokenHash string, ttl time.Duration) error
	SessionUser(ctx context.Context, tokenHash string) (login string, err error)
	UpsertStack(ctx context.Context, s Stack) (stackID int64, err error)
	InsertVersion(ctx context.Context, v Version) error
	Search(ctx context.Context, q, harness, tag string) ([]StackWithLatest, error)
	GetStack(ctx context.Context, owner, name string) (Stack, []Version, error)
	GetVersion(ctx context.Context, owner, name string, v int) (Version, error)
	AllVersionRefs(ctx context.Context) ([]VersionRef, error)
	Close() error
}

var ErrVersionExists = errors.New("version already exists")
var ErrNotFound = errors.New("not found")
var ErrGitHubIdentityConflict = errors.New("github login is already bound to another identity")
var ErrGitHubRenameBlocked = errors.New("github login rename is blocked while the user owns stacks")
