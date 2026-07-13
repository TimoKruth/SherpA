package store

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func OpenPostgres(ctx context.Context, dsn string, maxConns ...int) (*PostgresStore, error) {
	poolConfig, err := postgresPoolConfig(dsn, maxConns...)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &PostgresStore{pool: pool}, nil
}

func postgresPoolConfig(dsn string, maxConns ...int) (*pgxpool.Config, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if len(maxConns) > 0 && maxConns[0] > 0 {
		if uint64(maxConns[0]) > uint64(^uint32(0)>>1) {
			return nil, fmt.Errorf("database max connections exceeds supported range")
		}
		config.MaxConns = int32(maxConns[0])
	}
	return config, nil
}

func (s *PostgresStore) UpsertUser(ctx context.Context, handle string) (int64, error) {
	return upsertUser(ctx, s.pool, handle)
}

func (s *PostgresStore) UpsertUserGitHub(ctx context.Context, login string, githubID int64) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var id int64
	var currentLogin string
	err = tx.QueryRow(ctx, `
		select id, handle from users where github_id = $1 for update
	`, githubID).Scan(&id, &currentLogin)
	switch {
	case err == nil:
		if currentLogin != login {
			var ownsStacks bool
			if err := tx.QueryRow(ctx, `
				select exists(select 1 from stacks where owner_id = $1)
			`, id).Scan(&ownsStacks); err != nil {
				return 0, err
			}
			if ownsStacks {
				return 0, ErrGitHubRenameBlocked
			}

			var conflictingID int64
			err := tx.QueryRow(ctx, `
				select id from users where handle = $1 for update
			`, login).Scan(&conflictingID)
			if err == nil && conflictingID != id {
				return 0, ErrGitHubIdentityConflict
			}
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return 0, err
			}

			if _, err := tx.Exec(ctx, `update users set handle = $1 where id = $2`, login, id); err != nil {
				if isUniqueViolation(err) {
					return 0, ErrGitHubIdentityConflict
				}
				return 0, err
			}
		}
	case errors.Is(err, pgx.ErrNoRows):
		var boundGitHubID int64
		err = tx.QueryRow(ctx, `
			select id, coalesce(github_id, 0) from users where handle = $1 for update
		`, login).Scan(&id, &boundGitHubID)
		switch {
		case err == nil:
			if boundGitHubID != 0 && boundGitHubID != githubID {
				return 0, ErrGitHubIdentityConflict
			}
			if _, err := tx.Exec(ctx, `update users set github_id = $1 where id = $2`, githubID, id); err != nil {
				if isUniqueViolation(err) {
					return 0, ErrGitHubIdentityConflict
				}
				return 0, err
			}
		case errors.Is(err, pgx.ErrNoRows):
			if err := tx.QueryRow(ctx, `
				insert into users(handle, github_id) values ($1, $2) returning id
			`, login, githubID).Scan(&id); err != nil {
				if isUniqueViolation(err) {
					return 0, ErrGitHubIdentityConflict
				}
				return 0, err
			}
		default:
			return 0, err
		}
	default:
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *PostgresStore) CreateSession(ctx context.Context, userID int64, tokenHash string, purpose SessionPurpose, ttl time.Duration) error {
	if !purpose.Valid() {
		return ErrInvalidSessionPurpose
	}
	_, err := s.pool.Exec(ctx, `
		insert into sessions(user_id, token_hash, purpose, expires_at)
		values ($1, $2, $3, now() + $4::interval)
	`, userID, tokenHash, purpose, pgInterval(ttl))
	if isUniqueViolation(err) {
		return ErrSessionTokenConflict
	}
	return err
}

func (s *PostgresStore) SessionIdentity(ctx context.Context, tokenHash string) (SessionIdentity, error) {
	var identity SessionIdentity
	err := s.pool.QueryRow(ctx, `
		update sessions se set last_used_at = now()
		from users u
		where se.user_id = u.id and se.token_hash = $1 and se.expires_at > now()
		returning se.id, u.id, u.handle, se.purpose
	`, tokenHash).Scan(&identity.SessionID, &identity.UserID, &identity.Login, &identity.Purpose)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionIdentity{}, ErrNotFound
	}
	return identity, err
}

func (s *PostgresStore) RevokeSession(ctx context.Context, sessionID int64) error {
	_, err := s.pool.Exec(ctx, `delete from sessions where id = $1`, sessionID)
	return err
}

func (s *PostgresStore) CreateWebGrant(ctx context.Context, userID int64, grantHash, handoffChallenge string, ttl time.Duration) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		delete from web_grants
		where ctid in (
			select ctid from web_grants
			where expires_at <= now()
			order by expires_at
			limit 100
		)
	`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		insert into web_grants(token_hash, user_id, handoff_challenge, expires_at)
		values ($1, $2, $3, now() + $4::interval)
	`, grantHash, userID, handoffChallenge, pgInterval(ttl)); err != nil {
		if isUniqueViolation(err) {
			return ErrWebGrantConflict
		}
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) ExchangeWebGrant(ctx context.Context, grantHash, handoffChallenge, sessionHash string, ttl time.Duration) (SessionIdentity, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SessionIdentity{}, err
	}
	defer tx.Rollback(ctx)

	var identity SessionIdentity
	var storedChallenge string
	err = tx.QueryRow(ctx, `
		select wg.user_id, u.handle, wg.handoff_challenge
		from web_grants wg
		join users u on u.id = wg.user_id
		where wg.token_hash = $1 and wg.expires_at > now()
		for update
	`, grantHash).Scan(&identity.UserID, &identity.Login, &storedChallenge)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionIdentity{}, ErrWebGrantUnavailable
	}
	if err != nil {
		return SessionIdentity{}, err
	}
	if subtle.ConstantTimeCompare([]byte(storedChallenge), []byte(handoffChallenge)) != 1 {
		return SessionIdentity{}, ErrWebGrantUnavailable
	}

	if _, err := tx.Exec(ctx, `delete from web_grants where token_hash = $1`, grantHash); err != nil {
		return SessionIdentity{}, err
	}
	identity.Purpose = SessionWeb
	err = tx.QueryRow(ctx, `
		insert into sessions(user_id, token_hash, purpose, expires_at)
		values ($1, $2, $3, now() + $4::interval)
		returning id
	`, identity.UserID, sessionHash, identity.Purpose, pgInterval(ttl)).Scan(&identity.SessionID)
	if isUniqueViolation(err) {
		return SessionIdentity{}, ErrSessionTokenConflict
	}
	if err != nil {
		return SessionIdentity{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SessionIdentity{}, err
	}
	return identity, nil
}

func (s *PostgresStore) FollowStack(ctx context.Context, userID int64, owner, name string) (Follow, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Follow{}, err
	}
	defer tx.Rollback(ctx)

	var stackID, latestVersionID int64
	err = tx.QueryRow(ctx, `
		select s.id, sv.id
		from stacks s
		join users u on u.id = s.owner_id
		join lateral (
			select id from stack_versions where stack_id = s.id order by version desc limit 1
		) sv on true
		where u.handle = $1 and s.name = $2
	`, owner, name).Scan(&stackID, &latestVersionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Follow{}, ErrNotFound
	}
	if err != nil {
		return Follow{}, err
	}
	if _, err := tx.Exec(ctx, `
		insert into follows(user_id, stack_id, last_seen_version_id)
		values ($1, $2, $3)
		on conflict (user_id, stack_id) do nothing
	`, userID, stackID, latestVersionID); err != nil {
		return Follow{}, err
	}
	follow, err := getFollow(ctx, tx, userID, stackID)
	if err != nil {
		return Follow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Follow{}, err
	}
	return follow, nil
}

func (s *PostgresStore) GetFollow(ctx context.Context, userID int64, owner, name string) (Follow, error) {
	row := s.pool.QueryRow(ctx, `
		select s.id, owner.handle, s.name, coalesce(s.summary, ''), coalesce(s.harness, ''),
			coalesce(s.tags, array[]::text[]), latest.version, coalesce(latest.git_tag, ''),
			coalesce(latest.trust_tier, 'unreviewed'), latest.published_at,
			coalesce(seen.version, 0), (select count(*) from follows fc where fc.stack_id = s.id),
			f.created_at
		from follows f
		join stacks s on s.id = f.stack_id
		join users owner on owner.id = s.owner_id
		join lateral (
			select version, git_tag, trust_tier, published_at
			from stack_versions where stack_id = s.id order by version desc limit 1
		) latest on true
		left join stack_versions seen on seen.id = f.last_seen_version_id
		where f.user_id = $1 and owner.handle = $2 and s.name = $3
	`, userID, owner, name)
	follow, err := scanFollow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Follow{}, ErrNotFound
	}
	return follow, err
}

func (s *PostgresStore) UnfollowStack(ctx context.Context, userID int64, owner, name string) error {
	_, err := s.pool.Exec(ctx, `
		delete from follows f
		using stacks s, users u
		where f.user_id = $1 and f.stack_id = s.id and s.owner_id = u.id
			and u.handle = $2 and s.name = $3
	`, userID, owner, name)
	return err
}

func (s *PostgresStore) ListFollows(ctx context.Context, userID int64, page FollowPage) ([]Follow, error) {
	rows, err := s.pool.Query(ctx, `
		select s.id, owner.handle, s.name, coalesce(s.summary, ''), coalesce(s.harness, ''),
			coalesce(s.tags, array[]::text[]), latest.version, coalesce(latest.git_tag, ''),
			coalesce(latest.trust_tier, 'unreviewed'), latest.published_at,
			coalesce(seen.version, 0), (select count(*) from follows fc where fc.stack_id = s.id),
			f.created_at
		from follows f
		join stacks s on s.id = f.stack_id
		join users owner on owner.id = s.owner_id
		join lateral (
			select version, git_tag, trust_tier, published_at
			from stack_versions where stack_id = s.id order by version desc limit 1
		) latest on true
		left join stack_versions seen on seen.id = f.last_seen_version_id
		where f.user_id = $1
			and ($2 = '' or (owner.handle, s.name) > ($2, $3))
		order by owner.handle, s.name
		limit nullif($4, 0)
	`, userID, page.AfterOwner, page.AfterName, page.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var follows []Follow
	for rows.Next() {
		follow, err := scanFollow(rows)
		if err != nil {
			return nil, err
		}
		follows = append(follows, follow)
	}
	return follows, rows.Err()
}

func (s *PostgresStore) ListUpdates(ctx context.Context, userID int64, page UpdatePage) ([]Update, error) {
	rows, err := s.pool.Query(ctx, `
		select e.id, owner.handle, s.name, sv.version, coalesce(sv.git_tag, ''),
			coalesce(sv.changelog, ''), coalesce(sv.trust_tier, 'unreviewed'), sv.published_at,
			coalesce(seen.version, 0)
		from follows f
		join stacks s on s.id = f.stack_id
		join users owner on owner.id = s.owner_id
		join stack_versions sv on sv.stack_id = s.id
		join events e on e.stack_version_id = sv.id and e.type = 'stack_published'
		left join stack_versions seen on seen.id = f.last_seen_version_id
		where f.user_id = $1 and sv.version > coalesce(seen.version, 0)
			and ($2 = 0 or e.id < $2)
		order by e.id desc
		limit nullif($3, 0)
	`, userID, page.BeforeEventID, page.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var updates []Update
	for rows.Next() {
		var update Update
		if err := rows.Scan(
			&update.EventID, &update.Owner, &update.Name, &update.Version, &update.GitTag,
			&update.Changelog, &update.TrustTier, &update.PublishedAt, &update.SeenVersion,
		); err != nil {
			return nil, err
		}
		updates = append(updates, update)
	}
	return updates, rows.Err()
}

func (s *PostgresStore) MarkSeen(ctx context.Context, userID int64, owner, name string, version int) (Follow, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Follow{}, err
	}
	defer tx.Rollback(ctx)

	var stackID, versionID int64
	err = tx.QueryRow(ctx, `
		select s.id, sv.id
		from stacks s
		join users u on u.id = s.owner_id
		join stack_versions sv on sv.stack_id = s.id and sv.version = $3
		where u.handle = $1 and s.name = $2
	`, owner, name, version).Scan(&stackID, &versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Follow{}, ErrNotFound
	}
	if err != nil {
		return Follow{}, err
	}
	command, err := tx.Exec(ctx, `
		update follows f set last_seen_version_id = $3
		where f.user_id = $1 and f.stack_id = $2
			and coalesce((select version from stack_versions where id = f.last_seen_version_id), 0) < $4
	`, userID, stackID, versionID, version)
	if err != nil {
		return Follow{}, err
	}
	if command.RowsAffected() == 0 {
		var exists bool
		if err := tx.QueryRow(ctx, `select exists(select 1 from follows where user_id = $1 and stack_id = $2)`, userID, stackID).Scan(&exists); err != nil {
			return Follow{}, err
		}
		if !exists {
			return Follow{}, ErrNotFound
		}
	}
	follow, err := getFollow(ctx, tx, userID, stackID)
	if err != nil {
		return Follow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Follow{}, err
	}
	return follow, nil
}

func (s *PostgresStore) PutTrialFeedback(ctx context.Context, userID int64, owner, name string, version int, verdict Verdict) error {
	if !verdict.Valid() {
		return ErrInvalidVerdict
	}
	command, err := s.pool.Exec(ctx, `
		insert into trial_feedback(user_id, stack_version_id, verdict)
		select $1, sv.id, $5
		from stack_versions sv
		join stacks s on s.id = sv.stack_id
		join users u on u.id = s.owner_id
		where u.handle = $2 and s.name = $3 and sv.version = $4
		on conflict (user_id, stack_version_id) do update set
			verdict = excluded.verdict, updated_at = now()
	`, userID, owner, name, version, verdict)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) GetUser(ctx context.Context, handle string, maxRows, offset int) (UserProfile, []StackWithLatest, error) {
	var profile UserProfile
	var userID int64
	err := s.pool.QueryRow(ctx, `
		select u.id, u.handle, coalesce((
			select count(*) from follows f join stacks s on s.id = f.stack_id where s.owner_id = u.id
		), 0)
		from users u where u.handle = $1
	`, handle).Scan(&userID, &profile.Handle, &profile.TotalStackFollows)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserProfile{}, nil, ErrNotFound
	}
	if err != nil {
		return UserProfile{}, nil, err
	}
	rows, err := s.pool.Query(ctx, `
		select s.id, u.handle, s.name, coalesce(s.summary, ''), coalesce(s.harness, ''),
			coalesce(s.forked_from, ''), coalesce(s.tags, array[]::text[]),
			(select count(*) from follows f where f.stack_id = s.id), s.created_at,
			v.version, coalesce(v.git_tag, ''), coalesce(v.trust_tier, 'unreviewed'), v.published_at
		from stacks s
		join users u on u.id = s.owner_id
		join lateral (
			select version, git_tag, trust_tier, published_at
			from stack_versions where stack_id = s.id order by version desc limit 1
		) v on true
		where s.owner_id = $1
		order by v.published_at desc, s.name
		limit nullif($2, 0) offset $3
	`, userID, maxRows, offset)
	if err != nil {
		return UserProfile{}, nil, err
	}
	defer rows.Close()
	stacks, err := scanStacksWithLatest(rows)
	if err != nil {
		return UserProfile{}, nil, err
	}
	return profile, stacks, nil
}

func getFollow(ctx context.Context, db queryRower, userID, stackID int64) (Follow, error) {
	row := db.QueryRow(ctx, `
		select s.id, owner.handle, s.name, coalesce(s.summary, ''), coalesce(s.harness, ''),
			coalesce(s.tags, array[]::text[]), latest.version, coalesce(latest.git_tag, ''),
			coalesce(latest.trust_tier, 'unreviewed'), latest.published_at,
			coalesce(seen.version, 0), (select count(*) from follows fc where fc.stack_id = s.id),
			f.created_at
		from follows f
		join stacks s on s.id = f.stack_id
		join users owner on owner.id = s.owner_id
		join lateral (
			select version, git_tag, trust_tier, published_at
			from stack_versions where stack_id = s.id order by version desc limit 1
		) latest on true
		left join stack_versions seen on seen.id = f.last_seen_version_id
		where f.user_id = $1 and f.stack_id = $2
	`, userID, stackID)
	follow, err := scanFollow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Follow{}, ErrNotFound
	}
	return follow, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanFollow(row rowScanner) (Follow, error) {
	var follow Follow
	err := row.Scan(
		&follow.StackID, &follow.Owner, &follow.Name, &follow.Summary, &follow.Harness,
		&follow.Tags, &follow.LatestVersion, &follow.LatestGitTag, &follow.LatestTrustTier,
		&follow.LatestPublishedAt, &follow.LastSeenVersion, &follow.FollowerCount, &follow.CreatedAt,
	)
	return follow, err
}

func pgInterval(ttl time.Duration) string {
	return fmt.Sprintf("%f seconds", ttl.Seconds())
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func upsertUser(ctx context.Context, db queryRower, handle string) (int64, error) {
	var id int64
	err := db.QueryRow(ctx, `
		insert into users(handle)
		values ($1)
		on conflict (handle) do update set handle = excluded.handle
		returning id
	`, handle).Scan(&id)
	return id, err
}

func (s *PostgresStore) UpsertStack(ctx context.Context, stack Stack) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	ownerID, err := upsertUser(ctx, tx, stack.Owner)
	if err != nil {
		return 0, err
	}

	var id int64
	err = tx.QueryRow(ctx, `
		insert into stacks(owner_id, name, summary, tags, harness, forked_from)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (owner_id, name) do update set
			summary = excluded.summary,
			tags = excluded.tags,
			harness = excluded.harness,
			forked_from = excluded.forked_from
		returning id
	`, ownerID, stack.Name, stack.Summary, stack.Tags, stack.Harness, stack.ForkedFrom).Scan(&id)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *PostgresStore) InsertVersion(ctx context.Context, version Version) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var versionID int64
	err = tx.QueryRow(ctx, `
		insert into stack_versions(stack_id, version, git_tag, manifest, scan_report, changelog, trust_tier)
		values ($1, $2, $3, $4, $5, $6, $7)
		returning id
	`, version.StackID, version.Version, version.GitTag, jsonOrNil(version.Manifest), jsonOrNil(version.ScanReport), version.Changelog, version.TrustTier).Scan(&versionID)
	if isUniqueViolation(err) {
		return ErrVersionExists
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		insert into events(type, stack_version_id) values ('stack_published', $1)
	`, versionID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) Search(ctx context.Context, q, harness, tag string, maxRows, offset int) ([]StackWithLatest, error) {
	rows, err := s.pool.Query(ctx, `
		select
			s.id, u.handle, s.name, coalesce(s.summary, ''), coalesce(s.harness, ''),
			coalesce(s.forked_from, ''), coalesce(s.tags, array[]::text[]),
			(select count(*) from follows f where f.stack_id = s.id), s.created_at,
			v.version, coalesce(v.git_tag, ''), coalesce(v.trust_tier, 'unreviewed'), v.published_at
		from stacks s
		join users u on u.id = s.owner_id
		join lateral (
			select version, git_tag, trust_tier, published_at
			from stack_versions
			where stack_id = s.id
			order by version desc
			limit 1
		) v on true
		where
			($1 = '' or s.name ilike '%' || $1 || '%' or coalesce(s.summary, '') ilike '%' || $1 || '%'
				or exists (select 1 from unnest(coalesce(s.tags, array[]::text[])) t where t ilike '%' || $1 || '%'))
			and ($2 = '' or lower(s.harness) = lower($2))
			and ($3 = '' or exists (select 1 from unnest(coalesce(s.tags, array[]::text[])) t where lower(t) = lower($3)))
		order by v.published_at desc, u.handle, s.name
		limit nullif($4, 0) offset $5
	`, q, harness, tag, maxRows, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var matches []StackWithLatest
	for rows.Next() {
		var match StackWithLatest
		if err := rows.Scan(
			&match.ID,
			&match.Owner,
			&match.Name,
			&match.Summary,
			&match.Harness,
			&match.ForkedFrom,
			&match.Tags,
			&match.FollowerCount,
			&match.CreatedAt,
			&match.Version,
			&match.GitTag,
			&match.TrustTier,
			&match.PublishedAt,
		); err != nil {
			return nil, err
		}
		matches = append(matches, match)
	}
	return matches, rows.Err()
}

func scanStacksWithLatest(rows pgx.Rows) ([]StackWithLatest, error) {
	var stacks []StackWithLatest
	for rows.Next() {
		var match StackWithLatest
		if err := rows.Scan(
			&match.ID,
			&match.Owner,
			&match.Name,
			&match.Summary,
			&match.Harness,
			&match.ForkedFrom,
			&match.Tags,
			&match.FollowerCount,
			&match.CreatedAt,
			&match.Version,
			&match.GitTag,
			&match.TrustTier,
			&match.PublishedAt,
		); err != nil {
			return nil, err
		}
		stacks = append(stacks, match)
	}
	return stacks, rows.Err()
}

func (s *PostgresStore) GetStack(ctx context.Context, owner, name string, maxVersions, offset int) (Stack, []Version, error) {
	var stack Stack
	err := s.pool.QueryRow(ctx, `
		select s.id, u.handle, s.name, coalesce(s.summary, ''), coalesce(s.harness, ''),
			coalesce(s.forked_from, ''), coalesce(s.tags, array[]::text[]),
			(select count(*) from follows f where f.stack_id = s.id), s.created_at
		from stacks s
		join users u on u.id = s.owner_id
		where u.handle = $1 and s.name = $2
	`, owner, name).Scan(
		&stack.ID,
		&stack.Owner,
		&stack.Name,
		&stack.Summary,
		&stack.Harness,
		&stack.ForkedFrom,
		&stack.Tags,
		&stack.FollowerCount,
		&stack.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Stack{}, nil, ErrNotFound
	}
	if err != nil {
		return Stack{}, nil, err
	}

	versions, err := s.versionsForStack(ctx, stack.ID, maxVersions, offset)
	if err != nil {
		return Stack{}, nil, err
	}
	return stack, versions, nil
}

func (s *PostgresStore) GetVersion(ctx context.Context, owner, name string, v int) (Version, error) {
	var version Version
	err := s.pool.QueryRow(ctx, `
		select sv.id, sv.stack_id, sv.version, coalesce(sv.git_tag, ''), sv.manifest,
			sv.scan_report, coalesce(sv.changelog, ''), coalesce(sv.trust_tier, 'unreviewed'), sv.published_at
		from stack_versions sv
		join stacks s on s.id = sv.stack_id
		join users u on u.id = s.owner_id
		where u.handle = $1 and s.name = $2 and sv.version = $3
	`, owner, name, v).Scan(
		&version.ID,
		&version.StackID,
		&version.Version,
		&version.GitTag,
		&version.Manifest,
		&version.ScanReport,
		&version.Changelog,
		&version.TrustTier,
		&version.PublishedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, ErrNotFound
	}
	return version, err
}

func (s *PostgresStore) AllVersionRefs(ctx context.Context) ([]VersionRef, error) {
	rows, err := s.pool.Query(ctx, `
		select u.handle, s.name, sv.version, coalesce(sv.git_tag, '')
		from stack_versions sv
		join stacks s on s.id = sv.stack_id
		join users u on u.id = s.owner_id
		order by u.handle, s.name, sv.version, sv.git_tag
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var refs []VersionRef
	for rows.Next() {
		var ref VersionRef
		if err := rows.Scan(&ref.Owner, &ref.Name, &ref.Version, &ref.GitTag); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

func (s *PostgresStore) Close() error {
	if s == nil || s.pool == nil {
		return nil
	}
	s.pool.Close()
	return nil
}

func (s *PostgresStore) versionsForStack(ctx context.Context, stackID int64, maxVersions, offset int) ([]Version, error) {
	rows, err := s.pool.Query(ctx, `
		select id, stack_id, version, coalesce(git_tag, ''), manifest, scan_report,
			coalesce(changelog, ''), coalesce(trust_tier, 'unreviewed'), published_at
		from stack_versions
		where stack_id = $1
		order by version desc
		limit nullif($2, 0) offset $3
	`, stackID, maxVersions, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var versions []Version
	for rows.Next() {
		var version Version
		if err := rows.Scan(
			&version.ID,
			&version.StackID,
			&version.Version,
			&version.GitTag,
			&version.Manifest,
			&version.ScanReport,
			&version.Changelog,
			&version.TrustTier,
			&version.PublishedAt,
		); err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}
	return versions, rows.Err()
}

func jsonOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
