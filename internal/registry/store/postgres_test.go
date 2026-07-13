package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresPoolConfigMaxConns(t *testing.T) {
	config, err := postgresPoolConfig("postgres://localhost/sherpa", 9)
	if err != nil {
		t.Fatalf("postgresPoolConfig: %v", err)
	}
	if config.MaxConns != 9 {
		t.Fatalf("MaxConns = %d, want 9", config.MaxConns)
	}
}

func TestMigrationsUpgradeLegacySessionsAndRemainIdempotent(t *testing.T) {
	dsn := StartPostgres(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		create table users (
			id bigserial primary key,
			handle text unique not null,
			display_name text,
			created_at timestamptz default now()
		);
		create table sessions (
			id bigserial primary key,
			user_id bigint references users(id),
			token_hash text unique not null,
			created_at timestamptz default now(),
			last_used_at timestamptz,
			expires_at timestamptz not null
		);
		insert into users(handle) values ('legacy');
		insert into sessions(user_id, token_hash, expires_at)
		select id, 'legacy-hash', now() + interval '1 hour' from users where handle = 'legacy';
	`); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	pool.Close()

	st, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var purpose SessionPurpose
	if err := st.pool.QueryRow(ctx, `select purpose from sessions where token_hash = 'legacy-hash'`).Scan(&purpose); err != nil {
		t.Fatal(err)
	}
	if purpose != SessionCLI {
		t.Fatalf("legacy purpose = %q, want %q", purpose, SessionCLI)
	}
	st.Close()

	st, err = OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("second migration run: %v", err)
	}
	defer st.Close()

	for _, table := range []string{"follows", "events", "web_grants", "trial_feedback"} {
		var exists bool
		if err := st.pool.QueryRow(ctx, `select to_regclass('public.' || $1) is not null`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("table %q was not created", table)
		}
	}
	var purposeConstraints int
	if err := st.pool.QueryRow(ctx, `
		select count(*) from pg_constraint
		where conname = 'sessions_purpose_check' and conrelid = 'sessions'::regclass
	`).Scan(&purposeConstraints); err != nil {
		t.Fatal(err)
	}
	if purposeConstraints != 1 {
		t.Fatalf("sessions_purpose_check count = %d, want 1", purposeConstraints)
	}
	for _, index := range []string{
		"follows_pkey",
		"follows_stack_id_idx",
		"events_pkey",
		"events_stack_version_id_idx",
		"web_grants_pkey",
		"web_grants_expires_at_idx",
		"trial_feedback_pkey",
	} {
		var exists bool
		if err := st.pool.QueryRow(ctx, `select to_regclass('public.' || $1) is not null`, index).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("index %q was not created", index)
		}
	}
}

func TestPostgresSessionAndWebGrantLifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := OpenPostgres(ctx, StartPostgres(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	userID, err := st.UpsertUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, userID, "bad-purpose", SessionPurpose("admin"), time.Hour); !errors.Is(err, ErrInvalidSessionPurpose) {
		t.Fatalf("invalid purpose error = %v", err)
	}
	if err := st.CreateSession(ctx, userID, "cli-hash", SessionCLI, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, userID, "cli-hash", SessionCLI, time.Hour); !errors.Is(err, ErrSessionTokenConflict) || strings.Contains(err.Error(), "cli-hash") {
		t.Fatalf("duplicate session error = %v", err)
	}
	cliIdentity, err := st.SessionIdentity(ctx, "cli-hash")
	if err != nil {
		t.Fatal(err)
	}
	if cliIdentity.UserID != userID || cliIdentity.Login != "alice" || cliIdentity.Purpose != SessionCLI || cliIdentity.SessionID == 0 {
		t.Fatalf("CLI identity = %#v", cliIdentity)
	}
	var lastUsedAt *time.Time
	if err := st.pool.QueryRow(ctx, `select last_used_at from sessions where id = $1`, cliIdentity.SessionID).Scan(&lastUsedAt); err != nil {
		t.Fatal(err)
	}
	if lastUsedAt == nil {
		t.Fatal("SessionIdentity did not update last_used_at")
	}
	if err := st.RevokeSession(ctx, cliIdentity.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeSession(ctx, cliIdentity.SessionID); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if _, err := st.SessionIdentity(ctx, "cli-hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked session error = %v", err)
	}

	if err := st.CreateWebGrant(ctx, userID, "expired-grant", "expired-challenge", -time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ExchangeWebGrant(ctx, "expired-grant", "expired-challenge", "unused-session", time.Hour); !errors.Is(err, ErrWebGrantUnavailable) {
		t.Fatalf("expired grant error = %v", err)
	}
	if err := st.CreateWebGrant(ctx, userID, "cleanup-trigger", "cleanup-challenge", time.Hour); err != nil {
		t.Fatal(err)
	}
	var expiredGrants int
	if err := st.pool.QueryRow(ctx, `select count(*) from web_grants where expires_at <= now()`).Scan(&expiredGrants); err != nil {
		t.Fatal(err)
	}
	if expiredGrants != 0 {
		t.Fatalf("expired grants after opportunistic cleanup = %d", expiredGrants)
	}
	if err := st.CreateWebGrant(ctx, userID, "valid-grant", "valid-challenge", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWebGrant(ctx, userID, "valid-grant", "valid-challenge", time.Hour); !errors.Is(err, ErrWebGrantConflict) || strings.Contains(err.Error(), "valid-grant") {
		t.Fatalf("duplicate grant error = %v", err)
	}
	if _, err := st.ExchangeWebGrant(ctx, "valid-grant", "wrong-challenge", "wrong-session", time.Hour); !errors.Is(err, ErrWebGrantUnavailable) {
		t.Fatalf("wrong challenge error = %v", err)
	}
	webIdentity, err := st.ExchangeWebGrant(ctx, "valid-grant", "valid-challenge", "web-session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if webIdentity.UserID != userID || webIdentity.Login != "alice" || webIdentity.Purpose != SessionWeb || webIdentity.SessionID == 0 {
		t.Fatalf("web identity = %#v", webIdentity)
	}
	if got, err := st.SessionIdentity(ctx, "web-session"); err != nil || got != webIdentity {
		t.Fatalf("stored web identity = %#v, %v; want %#v", got, err, webIdentity)
	}
	if _, err := st.ExchangeWebGrant(ctx, "valid-grant", "valid-challenge", "replay-session", time.Hour); !errors.Is(err, ErrWebGrantUnavailable) {
		t.Fatalf("replayed grant error = %v", err)
	}

	if err := st.CreateSession(ctx, userID, "collision-session", SessionCLI, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWebGrant(ctx, userID, "rollback-grant", "rollback-challenge", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ExchangeWebGrant(ctx, "rollback-grant", "rollback-challenge", "collision-session", time.Hour); err == nil {
		t.Fatal("exchange with duplicate session hash succeeded")
	} else if !errors.Is(err, ErrSessionTokenConflict) || strings.Contains(err.Error(), "rollback-grant") || strings.Contains(err.Error(), "rollback-challenge") || strings.Contains(err.Error(), "collision-session") {
		t.Fatalf("exchange error leaked hash material: %v", err)
	}
	if _, err := st.ExchangeWebGrant(ctx, "rollback-grant", "rollback-challenge", "after-rollback-session", time.Hour); err != nil {
		t.Fatalf("grant was not restored by transaction rollback: %v", err)
	}

	if err := st.CreateWebGrant(ctx, userID, "concurrent-grant", "concurrent-challenge", time.Hour); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, sessionHash := range []string{"concurrent-session-a", "concurrent-session-b"} {
		wg.Add(1)
		go func(hash string) {
			defer wg.Done()
			<-start
			_, err := st.ExchangeWebGrant(ctx, "concurrent-grant", "concurrent-challenge", hash, time.Hour)
			errs <- err
		}(sessionHash)
	}
	close(start)
	wg.Wait()
	close(errs)
	var successes, unavailable int
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrWebGrantUnavailable):
			unavailable++
		default:
			t.Fatalf("concurrent exchange error = %v", err)
		}
	}
	if successes != 1 || unavailable != 1 {
		t.Fatalf("concurrent exchange successes=%d unavailable=%d", successes, unavailable)
	}
}

func TestInsertVersionAndPublishEventAreAtomic(t *testing.T) {
	ctx := context.Background()
	st, err := OpenPostgres(ctx, StartPostgres(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	stackID, err := st.UpsertStack(ctx, Stack{Owner: "alice", Name: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertVersion(ctx, Version{StackID: stackID, Version: 1, GitTag: "v1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertVersion(ctx, Version{StackID: stackID, Version: 1, GitTag: "v1"}); !errors.Is(err, ErrVersionExists) {
		t.Fatalf("duplicate version error = %v", err)
	}
	var events int
	if err := st.pool.QueryRow(ctx, `select count(*) from events where type = 'stack_published'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("event count = %d, want 1", events)
	}

	if _, err := st.pool.Exec(ctx, `
		create function reject_publish_event() returns trigger language plpgsql as $$
		begin
			raise exception 'injected event failure';
		end
		$$;
		create trigger reject_publish_event before insert on events
		for each row execute function reject_publish_event();
	`); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertVersion(ctx, Version{StackID: stackID, Version: 2, GitTag: "v2"}); err == nil {
		t.Fatal("insert version succeeded despite event failure")
	}
	if _, err := st.GetVersion(ctx, "alice", "reviewer", 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("version survived event rollback: %v", err)
	}
	if err := st.pool.QueryRow(ctx, `select count(*) from events where type = 'stack_published'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("event count after rollback = %d, want 1", events)
	}
}

func TestPostgresSocialContract(t *testing.T) {
	ctx := context.Background()
	st, err := OpenPostgres(ctx, StartPostgres(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	stackID, err := st.UpsertStack(ctx, Stack{Owner: "alice", Name: "reviewer", Summary: "Review code", Harness: "codex", Tags: []string{"review"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertVersion(ctx, Version{StackID: stackID, Version: 1, GitTag: "v1", Changelog: "initial", TrustTier: "linked"}); err != nil {
		t.Fatal(err)
	}
	secondStackID, err := st.UpsertStack(ctx, Stack{Owner: "alice", Name: "builder"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertVersion(ctx, Version{StackID: secondStackID, Version: 1, GitTag: "v1"}); err != nil {
		t.Fatal(err)
	}
	bobID, _ := st.UpsertUser(ctx, "bob")
	carolID, _ := st.UpsertUser(ctx, "carol")
	eveID, _ := st.UpsertUser(ctx, "eve")

	bobFollow, err := st.FollowStack(ctx, bobID, "alice", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if bobFollow.LatestVersion != 1 || bobFollow.LastSeenVersion != 1 || bobFollow.FollowerCount != 1 {
		t.Fatalf("initial follow = %#v", bobFollow)
	}
	duplicate, err := st.FollowStack(ctx, bobID, "alice", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.CreatedAt != bobFollow.CreatedAt || duplicate.FollowerCount != 1 {
		t.Fatalf("duplicate follow = %#v, first = %#v", duplicate, bobFollow)
	}
	if _, err := st.FollowStack(ctx, bobID, "alice", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing follow error = %v", err)
	}
	if _, err := st.FollowStack(ctx, carolID, "alice", "reviewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FollowStack(ctx, eveID, "alice", "reviewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FollowStack(ctx, bobID, "alice", "builder"); err != nil {
		t.Fatal(err)
	}

	updates, err := st.ListUpdates(ctx, bobID, UpdatePage{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 0 {
		t.Fatalf("new follow has historical updates: %#v", updates)
	}
	follows, err := st.ListFollows(ctx, bobID, FollowPage{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(follows) != 1 || follows[0].Name != "builder" {
		t.Fatalf("first follow page = %#v", follows)
	}
	follows, err = st.ListFollows(ctx, bobID, FollowPage{Limit: 2, AfterOwner: follows[0].Owner, AfterName: follows[0].Name})
	if err != nil {
		t.Fatal(err)
	}
	if len(follows) != 1 || follows[0].Name != "reviewer" || follows[0].FollowerCount != 3 {
		t.Fatalf("second follow page = %#v", follows)
	}

	for version := 2; version <= 3; version++ {
		if err := st.InsertVersion(ctx, Version{
			StackID: stackID, Version: version, GitTag: "v" + strconv.Itoa(version),
			Changelog: "release " + strconv.Itoa(version), TrustTier: "linked",
		}); err != nil {
			t.Fatal(err)
		}
	}
	updates, err = st.ListUpdates(ctx, bobID, UpdatePage{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 2 || updates[0].Version != 3 || updates[1].Version != 2 || updates[0].SeenVersion != 1 {
		t.Fatalf("pending updates = %#v", updates)
	}
	page, err := st.ListUpdates(ctx, bobID, UpdatePage{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].Version != 3 {
		t.Fatalf("first update page = %#v", page)
	}
	next, err := st.ListUpdates(ctx, bobID, UpdatePage{Limit: 1, BeforeEventID: page[0].EventID})
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || next[0].Version != 2 {
		t.Fatalf("second update page = %#v", next)
	}

	seen, err := st.MarkSeen(ctx, bobID, "alice", "reviewer", 2)
	if err != nil || seen.LastSeenVersion != 2 {
		t.Fatalf("mark v2 seen = %#v, %v", seen, err)
	}
	seen, err = st.MarkSeen(ctx, bobID, "alice", "reviewer", 1)
	if err != nil || seen.LastSeenVersion != 2 {
		t.Fatalf("backward seen = %#v, %v", seen, err)
	}
	if _, err := st.MarkSeen(ctx, bobID, "alice", "reviewer", 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing version seen error = %v", err)
	}
	if _, err := st.MarkSeen(ctx, carolID, "alice", "builder", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unfollowed stack seen error = %v", err)
	}

	start := make(chan struct{})
	seenResults := make(chan Follow, 2)
	errResults := make(chan error, 2)
	for _, version := range []int{2, 3} {
		go func(v int) {
			<-start
			follow, err := st.MarkSeen(ctx, eveID, "alice", "reviewer", v)
			seenResults <- follow
			errResults <- err
		}(version)
	}
	close(start)
	for range 2 {
		if err := <-errResults; err != nil {
			t.Fatalf("concurrent seen: %v", err)
		}
		<-seenResults
	}
	eveFollows, err := st.ListFollows(ctx, eveID, FollowPage{Limit: 10})
	if err != nil || len(eveFollows) != 1 || eveFollows[0].LastSeenVersion != 3 {
		t.Fatalf("concurrent seen result = %#v, %v", eveFollows, err)
	}

	if err := st.PutTrialFeedback(ctx, bobID, "alice", "reviewer", 3, Verdict("invalid")); !errors.Is(err, ErrInvalidVerdict) {
		t.Fatalf("invalid verdict error = %v", err)
	}
	if err := st.PutTrialFeedback(ctx, bobID, "alice", "reviewer", 99, VerdictKeep); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing feedback version error = %v", err)
	}
	if err := st.PutTrialFeedback(ctx, bobID, "alice", "reviewer", 3, VerdictKeep); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTrialFeedback(ctx, bobID, "alice", "reviewer", 3, VerdictRevert); err != nil {
		t.Fatal(err)
	}
	var verdict Verdict
	if err := st.pool.QueryRow(ctx, `select verdict from trial_feedback where user_id = $1`, bobID).Scan(&verdict); err != nil {
		t.Fatal(err)
	}
	if verdict != VerdictRevert {
		t.Fatalf("stored verdict = %q", verdict)
	}

	profile, stacks, err := st.GetUser(ctx, "alice", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Handle != "alice" || profile.TotalStackFollows != 4 || len(stacks) != 2 {
		t.Fatalf("profile = %#v, stacks = %#v", profile, stacks)
	}
	matches, err := st.Search(ctx, "reviewer", "", "", 10, 0)
	if err != nil || len(matches) != 1 || matches[0].FollowerCount != 3 {
		t.Fatalf("search follower count = %#v, %v", matches, err)
	}
	stack, _, err := st.GetStack(ctx, "alice", "reviewer", 1, 0)
	if err != nil || stack.FollowerCount != 3 {
		t.Fatalf("stack follower count = %#v, %v", stack, err)
	}

	if err := st.UnfollowStack(ctx, carolID, "alice", "reviewer"); err != nil {
		t.Fatal(err)
	}
	if err := st.UnfollowStack(ctx, carolID, "alice", "reviewer"); err != nil {
		t.Fatal(err)
	}
	if err := st.UnfollowStack(ctx, carolID, "alice", "missing"); err != nil {
		t.Fatal(err)
	}
	stack, _, err = st.GetStack(ctx, "alice", "reviewer", 1, 0)
	if err != nil || stack.FollowerCount != 2 {
		t.Fatalf("count after unfollow = %#v, %v", stack, err)
	}

	raceStackID, err := st.UpsertStack(ctx, Stack{Owner: "alice", Name: "race"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertVersion(ctx, Version{StackID: raceStackID, Version: 1, GitTag: "v1"}); err != nil {
		t.Fatal(err)
	}
	frankID, err := st.UpsertUser(ctx, "frank")
	if err != nil {
		t.Fatal(err)
	}
	raceStart := make(chan struct{})
	raceErrors := make(chan error, 2)
	go func() {
		<-raceStart
		_, err := st.FollowStack(ctx, frankID, "alice", "race")
		raceErrors <- err
	}()
	go func() {
		<-raceStart
		raceErrors <- st.InsertVersion(ctx, Version{StackID: raceStackID, Version: 2, GitTag: "v2"})
	}()
	close(raceStart)
	for range 2 {
		if err := <-raceErrors; err != nil {
			t.Fatalf("follow/publish race: %v", err)
		}
	}
	frankFollows, err := st.ListFollows(ctx, frankID, FollowPage{Limit: 10})
	if err != nil || len(frankFollows) != 1 {
		t.Fatalf("race follow = %#v, %v", frankFollows, err)
	}
	frankUpdates, err := st.ListUpdates(ctx, frankID, UpdatePage{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	switch frankFollows[0].LastSeenVersion {
	case 1:
		if len(frankUpdates) != 1 || frankUpdates[0].Version != 2 {
			t.Fatalf("v1 race baseline updates = %#v", frankUpdates)
		}
	case 2:
		if len(frankUpdates) != 0 {
			t.Fatalf("v2 race baseline updates = %#v", frankUpdates)
		}
	default:
		t.Fatalf("race baseline = %d", frankFollows[0].LastSeenVersion)
	}
}

func TestPostgresStoreContract(t *testing.T) {
	dsn := StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	store, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("open postgres store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	})

	userID, err := store.UpsertUser(ctx, "alice")
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	if userID == 0 {
		t.Fatalf("user id = 0")
	}

	stackID, err := store.UpsertStack(ctx, Stack{
		Owner:      "alice",
		Name:       "reviewer",
		Summary:    "Review code with strict security checks",
		Harness:    "claude-code",
		ForkedFrom: "origin/reviewer",
		Tags:       []string{"review", "security"},
	})
	if err != nil {
		t.Fatalf("upsert stack: %v", err)
	}
	if stackID == 0 {
		t.Fatalf("stack id = 0")
	}

	if err := store.InsertVersion(ctx, Version{
		StackID:    stackID,
		Version:    1,
		GitTag:     "v1",
		Manifest:   json.RawMessage(`{"name":"reviewer","version":1}`),
		ScanReport: json.RawMessage(`{"findings":[]}`),
		Changelog:  "Initial release",
	}); err != nil {
		t.Fatalf("insert version 1: %v", err)
	}
	if err := store.InsertVersion(ctx, Version{
		StackID:    stackID,
		Version:    2,
		GitTag:     "v2",
		Manifest:   json.RawMessage(`{"name":"reviewer","version":2}`),
		ScanReport: json.RawMessage(`{"findings":[]}`),
		Changelog:  "Second release",
	}); err != nil {
		t.Fatalf("insert version 2: %v", err)
	}
	if err := store.InsertVersion(ctx, Version{
		StackID:    stackID,
		Version:    2,
		GitTag:     "v2",
		Manifest:   json.RawMessage(`{"name":"reviewer","version":2}`),
		ScanReport: json.RawMessage(`{"findings":[]}`),
	}); !errors.Is(err, ErrVersionExists) {
		t.Fatalf("duplicate insert error = %v, want %v", err, ErrVersionExists)
	}

	matches, err := store.Search(ctx, "SECUR", "CLAUDE-CODE", "", 0, 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("search returned %d matches, want 1: %#v", len(matches), matches)
	}
	if matches[0].Owner != "alice" || matches[0].Name != "reviewer" || matches[0].Version != 2 {
		t.Fatalf("search match = %#v, want alice/reviewer latest v2", matches[0])
	}

	tagMatches, err := store.Search(ctx, "", "", "review", 0, 0)
	if err != nil {
		t.Fatalf("tag search: %v", err)
	}
	if len(tagMatches) != 1 || tagMatches[0].Version != 2 {
		t.Fatalf("tag search = %#v, want one latest v2 match", tagMatches)
	}

	noMatches, err := store.Search(ctx, "security", "codex", "", 0, 0)
	if err != nil {
		t.Fatalf("filtered search: %v", err)
	}
	if len(noMatches) != 0 {
		t.Fatalf("filtered search returned %#v, want none", noMatches)
	}

	stack, versions, err := store.GetStack(ctx, "alice", "reviewer", 0, 0)
	if err != nil {
		t.Fatalf("get stack: %v", err)
	}
	if stack.ID != stackID || stack.Owner != "alice" || stack.Name != "reviewer" {
		t.Fatalf("stack = %#v, want alice/reviewer id %d", stack, stackID)
	}
	if len(versions) != 2 || versions[0].Version != 2 || versions[1].Version != 1 {
		t.Fatalf("versions = %#v, want newest-first v2,v1", versions)
	}

	version, err := store.GetVersion(ctx, "alice", "reviewer", 1)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if version.StackID != stackID || version.Version != 1 || version.GitTag != "v1" {
		t.Fatalf("version = %#v, want stack %d v1 tag v1", version, stackID)
	}

	refs, err := store.AllVersionRefs(ctx)
	if err != nil {
		t.Fatalf("all version refs: %v", err)
	}
	if len(refs) != 2 || refs[0] != (VersionRef{Owner: "alice", Name: "reviewer", Version: 1, GitTag: "v1"}) || refs[1] != (VersionRef{Owner: "alice", Name: "reviewer", Version: 2, GitTag: "v2"}) {
		t.Fatalf("AllVersionRefs = %#v, want alice/reviewer v1,v2", refs)
	}

	if _, _, err := store.GetStack(ctx, "alice", "missing", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing stack error = %v, want %v", err, ErrNotFound)
	}
}

func TestPostgresReadPaginationAndStableOrdering(t *testing.T) {
	ctx := context.Background()
	st, err := OpenPostgres(ctx, StartPostgres(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for _, item := range []struct {
		owner string
		name  string
	}{
		{owner: "bob", name: "alpha"},
		{owner: "alice", name: "zeta"},
		{owner: "alice", name: "alpha"},
	} {
		stackID, err := st.UpsertStack(ctx, Stack{Owner: item.owner, Name: item.name})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.InsertVersion(ctx, Version{StackID: stackID, Version: 1, GitTag: "v1"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.pool.Exec(ctx, `update stack_versions set published_at = '2026-07-13T12:00:00Z'`); err != nil {
		t.Fatal(err)
	}

	all, err := st.Search(ctx, "", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := stackKeys(all); !equalStrings(got, []string{"alice/alpha", "alice/zeta", "bob/alpha"}) {
		t.Fatalf("stable order = %v", got)
	}
	page, err := st.Search(ctx, "", "", "", 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := stackKeys(page); !equalStrings(got, []string{"alice/zeta", "bob/alpha"}) {
		t.Fatalf("bounded search = %v", got)
	}

	stackID, err := st.UpsertStack(ctx, Stack{Owner: "carol", Name: "versions"})
	if err != nil {
		t.Fatal(err)
	}
	for version := 1; version <= 4; version++ {
		if err := st.InsertVersion(ctx, Version{StackID: stackID, Version: version, GitTag: "v" + strconv.Itoa(version)}); err != nil {
			t.Fatal(err)
		}
	}
	_, versions, err := st.GetStack(ctx, "carol", "versions", 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0].Version != 3 || versions[1].Version != 2 {
		t.Fatalf("bounded versions = %#v, want v3,v2", versions)
	}
	_, allVersions, err := st.GetStack(ctx, "carol", "versions", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(allVersions) != 4 || allVersions[0].Version != 4 || allVersions[3].Version != 1 {
		t.Fatalf("unbounded versions = %#v, want v4..v1", allVersions)
	}
}

func stackKeys(stacks []StackWithLatest) []string {
	keys := make([]string, len(stacks))
	for i, stack := range stacks {
		keys[i] = stack.Owner + "/" + stack.Name
	}
	return keys
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPostgresGitHubSessionsAndTrustTier(t *testing.T) {
	dsn := StartPostgres(t)
	ctx := context.Background()
	st, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	first, err := st.UpsertUserGitHub(ctx, "alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.UpsertUserGitHub(ctx, "alice-renamed", 42)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("user IDs = %d and %d", first, second)
	}
	if err := st.CreateSession(ctx, first, "valid-hash", SessionCLI, time.Hour); err != nil {
		t.Fatal(err)
	}
	if got, err := st.SessionIdentity(ctx, "valid-hash"); err != nil || got.Login != "alice-renamed" || got.Purpose != SessionCLI {
		t.Fatalf("SessionIdentity = %#v, %v", got, err)
	}
	if err := st.CreateSession(ctx, first, "expired-hash", SessionCLI, -time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SessionIdentity(ctx, "expired-hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired SessionIdentity error = %v", err)
	}
	stackID, err := st.UpsertStack(ctx, Stack{Owner: "alice-renamed", Name: "trusted"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertVersion(ctx, Version{StackID: stackID, Version: 1, GitTag: "v1", TrustTier: "linked"}); err != nil {
		t.Fatal(err)
	}
	v, err := st.GetVersion(ctx, "alice-renamed", "trusted", 1)
	if err != nil || v.TrustTier != "linked" {
		t.Fatalf("version = %#v, %v", v, err)
	}
	matches, err := st.Search(ctx, "trusted", "", "", 0, 0)
	if err != nil || len(matches) != 1 || matches[0].TrustTier != "linked" {
		t.Fatalf("search = %#v, %v", matches, err)
	}
}

func TestPostgresGitHubHandleCollisionDoesNotRebindIdentity(t *testing.T) {
	dsn := StartPostgres(t)
	ctx := context.Background()
	st, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	userID, err := st.UpsertUserGitHub(ctx, "alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, userID, "alice-session", SessionCLI, time.Hour); err != nil {
		t.Fatal(err)
	}

	if _, err := st.UpsertUserGitHub(ctx, "alice", 99); !errors.Is(err, ErrGitHubIdentityConflict) {
		t.Fatalf("colliding UpsertUserGitHub error = %v", err)
	}
	if got, err := st.SessionIdentity(ctx, "alice-session"); err != nil || got.Login != "alice" {
		t.Fatalf("SessionIdentity after collision = %#v, %v", got, err)
	}
	if got, err := st.UpsertUserGitHub(ctx, "alice", 42); err != nil || got != userID {
		t.Fatalf("original identity after collision = %d, %v", got, err)
	}
}

func TestPostgresGitHubRenameWithOwnedStackIsRejected(t *testing.T) {
	dsn := StartPostgres(t)
	ctx := context.Background()
	st, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	userID, err := st.UpsertUserGitHub(ctx, "alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertStack(ctx, Stack{Owner: "alice", Name: "published"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, userID, "alice-session", SessionCLI, time.Hour); err != nil {
		t.Fatal(err)
	}

	if _, err := st.UpsertUserGitHub(ctx, "alice-renamed", 42); !errors.Is(err, ErrGitHubRenameBlocked) {
		t.Fatalf("owned-stack rename error = %v", err)
	}
	if got, err := st.SessionIdentity(ctx, "alice-session"); err != nil || got.Login != "alice" {
		t.Fatalf("SessionIdentity after blocked rename = %#v, %v", got, err)
	}
	if _, _, err := st.GetStack(ctx, "alice", "published", 0, 0); err != nil {
		t.Fatalf("original stack after blocked rename: %v", err)
	}
	if _, _, err := st.GetStack(ctx, "alice-renamed", "published", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("renamed stack lookup error = %v", err)
	}
}
