package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type Stack struct {
	ID            int64
	Owner         string
	Name          string
	Summary       string
	Harness       string
	ForkedFrom    string
	Tags          []string
	FollowerCount int
	CreatedAt     time.Time
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

type SessionPurpose string

const (
	SessionCLI SessionPurpose = "cli"
	SessionWeb SessionPurpose = "web"
)

func (p SessionPurpose) Valid() bool {
	return p == SessionCLI || p == SessionWeb
}

type SessionIdentity struct {
	SessionID int64
	UserID    int64
	Login     string
	Purpose   SessionPurpose
}

type Follow struct {
	StackID           int64
	Owner             string
	Name              string
	Summary           string
	Harness           string
	Tags              []string
	LatestVersion     int
	LatestGitTag      string
	LatestTrustTier   string
	LatestPublishedAt time.Time
	LastSeenVersion   int
	FollowerCount     int
	CreatedAt         time.Time
}

type FollowPage struct {
	Limit      int
	AfterOwner string
	AfterName  string
}

type Update struct {
	EventID     int64
	Owner       string
	Name        string
	Version     int
	GitTag      string
	Changelog   string
	TrustTier   string
	PublishedAt time.Time
	SeenVersion int
}

type UpdatePage struct {
	Limit         int
	BeforeEventID int64
}

type Verdict string

const (
	VerdictKeep          Verdict = "keep"
	VerdictKeepWithNotes Verdict = "keep_with_notes"
	VerdictRevert        Verdict = "revert"
)

func (v Verdict) Valid() bool {
	return v == VerdictKeep || v == VerdictKeepWithNotes || v == VerdictRevert
}

type UserProfile struct {
	Handle            string
	TotalStackFollows int
}

type Store interface {
	UpsertUser(ctx context.Context, handle string) (userID int64, err error)
	UpsertUserGitHub(ctx context.Context, login string, githubID int64) (userID int64, err error)
	CreateSession(ctx context.Context, userID int64, tokenHash string, purpose SessionPurpose, ttl time.Duration) error
	SessionIdentity(ctx context.Context, tokenHash string) (SessionIdentity, error)
	RevokeSession(ctx context.Context, sessionID int64) error
	CreateWebGrant(ctx context.Context, userID int64, grantHash, handoffChallenge string, ttl time.Duration) error
	ExchangeWebGrant(ctx context.Context, grantHash, handoffChallenge, sessionHash string, ttl time.Duration) (SessionIdentity, error)
	FollowStack(ctx context.Context, userID int64, owner, name string) (Follow, error)
	GetFollow(ctx context.Context, userID int64, owner, name string) (Follow, error)
	UnfollowStack(ctx context.Context, userID int64, owner, name string) error
	ListFollows(ctx context.Context, userID int64, page FollowPage) ([]Follow, error)
	ListUpdates(ctx context.Context, userID int64, page UpdatePage) ([]Update, error)
	MarkSeen(ctx context.Context, userID int64, owner, name string, version int) (Follow, error)
	PutTrialFeedback(ctx context.Context, userID int64, owner, name string, version int, verdict Verdict) error
	GetUser(ctx context.Context, handle string, maxRows, offset int) (UserProfile, []StackWithLatest, error)
	UpsertStack(ctx context.Context, s Stack) (stackID int64, err error)
	InsertVersion(ctx context.Context, v Version) error
	Search(ctx context.Context, q, harness, tag string, maxRows, offset int) ([]StackWithLatest, error)
	GetStack(ctx context.Context, owner, name string, maxVersions, offset int) (Stack, []Version, error)
	GetVersion(ctx context.Context, owner, name string, v int) (Version, error)
	AllVersionRefs(ctx context.Context) ([]VersionRef, error)
	Close() error
}

var ErrVersionExists = errors.New("version already exists")
var ErrNotFound = errors.New("not found")
var ErrInvalidSessionPurpose = errors.New("invalid session purpose")
var ErrInvalidVerdict = errors.New("invalid verdict")
var ErrSessionTokenConflict = errors.New("session token conflict")
var ErrWebGrantConflict = errors.New("web grant conflict")
var ErrWebGrantUnavailable = errors.New("web grant unavailable")
var ErrGitHubIdentityConflict = errors.New("github login is already bound to another identity")
var ErrGitHubRenameBlocked = errors.New("github login rename is blocked while the user owns stacks")
