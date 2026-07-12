package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type PostgresStore struct {
	mu   sync.Mutex
	conn *pgx.Conn
}

func OpenPostgres(ctx context.Context, dsn string) (*PostgresStore, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close(ctx)
		return nil, err
	}
	if err := Migrate(ctx, conn); err != nil {
		_ = conn.Close(ctx)
		return nil, err
	}
	return &PostgresStore{conn: conn}, nil
}

func (s *PostgresStore) UpsertUser(ctx context.Context, handle string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upsertUserLocked(ctx, handle)
}

func (s *PostgresStore) upsertUserLocked(ctx context.Context, handle string) (int64, error) {
	var id int64
	err := s.conn.QueryRow(ctx, `
		insert into users(handle)
		values ($1)
		on conflict (handle) do update set handle = excluded.handle
		returning id
	`, handle).Scan(&id)
	return id, err
}

func (s *PostgresStore) UpsertStack(ctx context.Context, stack Stack) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ownerID, err := s.upsertUserLocked(ctx, stack.Owner)
	if err != nil {
		return 0, err
	}

	var id int64
	err = s.conn.QueryRow(ctx, `
		insert into stacks(owner_id, name, summary, tags, harness, forked_from)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (owner_id, name) do update set
			summary = excluded.summary,
			tags = excluded.tags,
			harness = excluded.harness,
			forked_from = excluded.forked_from
		returning id
	`, ownerID, stack.Name, stack.Summary, stack.Tags, stack.Harness, stack.ForkedFrom).Scan(&id)
	return id, err
}

func (s *PostgresStore) InsertVersion(ctx context.Context, version Version) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.conn.Exec(ctx, `
		insert into stack_versions(stack_id, version, git_tag, manifest, scan_report, changelog)
		values ($1, $2, $3, $4, $5, $6)
	`, version.StackID, version.Version, version.GitTag, jsonOrNil(version.Manifest), jsonOrNil(version.ScanReport), version.Changelog)
	if isUniqueViolation(err) {
		return ErrVersionExists
	}
	return err
}

func (s *PostgresStore) Search(ctx context.Context, q, harness, tag string) ([]StackWithLatest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.conn.Query(ctx, `
		select
			s.id, u.handle, s.name, coalesce(s.summary, ''), coalesce(s.harness, ''),
			coalesce(s.forked_from, ''), coalesce(s.tags, array[]::text[]), s.created_at,
			v.version, coalesce(v.git_tag, ''), v.published_at
		from stacks s
		join users u on u.id = s.owner_id
		join lateral (
			select version, git_tag, published_at
			from stack_versions
			where stack_id = s.id
			order by version desc
			limit 1
		) v on true
		where
			($1 = '' or s.name ilike '%' || $1 || '%' or coalesce(s.summary, '') ilike '%' || $1 || '%'
				or exists (select 1 from unnest(coalesce(s.tags, array[]::text[])) t where t ilike '%' || $1 || '%'))
			and ($2 = '' or s.harness = $2)
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
			&match.PublishedAt,
		); err != nil {
			return nil, err
		}
		matches = append(matches, match)
	}
	return matches, rows.Err()
}

func (s *PostgresStore) GetStack(ctx context.Context, owner, name string) (Stack, []Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var stack Stack
	err := s.conn.QueryRow(ctx, `
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

	versions, err := s.versionsForStackLocked(ctx, stack.ID)
	if err != nil {
		return Stack{}, nil, err
	}
	return stack, versions, nil
}

func (s *PostgresStore) GetVersion(ctx context.Context, owner, name string, v int) (Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var version Version
	err := s.conn.QueryRow(ctx, `
		select sv.id, sv.stack_id, sv.version, coalesce(sv.git_tag, ''), sv.manifest,
			sv.scan_report, coalesce(sv.changelog, ''), sv.published_at
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
		&version.PublishedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, ErrNotFound
	}
	return version, err
}

func (s *PostgresStore) Close() error {
	if s == nil || s.conn == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.Close(context.Background())
}

func (s *PostgresStore) versionsForStackLocked(ctx context.Context, stackID int64) ([]Version, error) {
	rows, err := s.conn.Query(ctx, `
		select id, stack_id, version, coalesce(git_tag, ''), manifest, scan_report,
			coalesce(changelog, ''), published_at
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
