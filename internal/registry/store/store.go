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
	PublishedAt time.Time
}

type StackWithLatest struct {
	Stack
	Version     int
	GitTag      string
	PublishedAt time.Time
}

type Store interface {
	UpsertUser(ctx context.Context, handle string) (userID int64, err error)
	UpsertStack(ctx context.Context, s Stack) (stackID int64, err error)
	InsertVersion(ctx context.Context, v Version) error
	Search(ctx context.Context, q, harness, tag string) ([]StackWithLatest, error)
	GetStack(ctx context.Context, owner, name string) (Stack, []Version, error)
	GetVersion(ctx context.Context, owner, name string, v int) (Version, error)
	Close() error
}

var ErrVersionExists = errors.New("version already exists")
var ErrNotFound = errors.New("not found")
