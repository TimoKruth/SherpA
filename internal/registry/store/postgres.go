package store

import (
	"context"
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

func (s *PostgresStore) CreateSession(ctx context.Context, userID int64, tokenHash string, ttl time.Duration) error {
	_, err := s.pool.Exec(ctx, `
		insert into sessions(user_id, token_hash, expires_at)
		values ($1, $2, now() + $3::interval)
	`, userID, tokenHash, pgInterval(ttl))
	return err
}

func (s *PostgresStore) SessionUser(ctx context.Context, tokenHash string) (string, error) {
	var login string
	err := s.pool.QueryRow(ctx, `
		update sessions se set last_used_at = now()
		from users u
		where se.user_id = u.id and se.token_hash = $1 and se.expires_at > now()
		returning u.handle
	`, tokenHash).Scan(&login)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return login, err
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
	_, err := s.pool.Exec(ctx, `
		insert into stack_versions(stack_id, version, git_tag, manifest, scan_report, changelog, trust_tier)
		values ($1, $2, $3, $4, $5, $6, $7)
	`, version.StackID, version.Version, version.GitTag, jsonOrNil(version.Manifest), jsonOrNil(version.ScanReport), version.Changelog, version.TrustTier)
	if isUniqueViolation(err) {
		return ErrVersionExists
	}
	return err
}

func (s *PostgresStore) Search(ctx context.Context, q, harness, tag string) ([]StackWithLatest, error) {
	rows, err := s.pool.Query(ctx, `
		select
			s.id, u.handle, s.name, coalesce(s.summary, ''), coalesce(s.harness, ''),
			coalesce(s.forked_from, ''), coalesce(s.tags, array[]::text[]), s.created_at,
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
		order by v.published_at desc, s.name
	`, q, harness, tag)
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

func (s *PostgresStore) GetStack(ctx context.Context, owner, name string) (Stack, []Version, error) {
	var stack Stack
	err := s.pool.QueryRow(ctx, `
		select s.id, u.handle, s.name, coalesce(s.summary, ''), coalesce(s.harness, ''),
			coalesce(s.forked_from, ''), coalesce(s.tags, array[]::text[]), s.created_at
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
		&stack.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Stack{}, nil, ErrNotFound
	}
	if err != nil {
		return Stack{}, nil, err
	}

	versions, err := s.versionsForStack(ctx, stack.ID)
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

func (s *PostgresStore) versionsForStack(ctx context.Context, stackID int64) ([]Version, error) {
	rows, err := s.pool.Query(ctx, `
		select id, stack_id, version, coalesce(git_tag, ''), manifest, scan_report,
			coalesce(changelog, ''), coalesce(trust_tier, 'unreviewed'), published_at
		from stack_versions
		where stack_id = $1
		order by version desc
	`, stackID)
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
