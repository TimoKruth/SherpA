package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type migrationDB interface {
	Begin(context.Context) (pgx.Tx, error)
}

var migrations = []string{
	`create table if not exists users (
		id bigserial primary key,
		handle text unique not null,
		display_name text,
		created_at timestamptz default now()
	)`,
	`create table if not exists stacks (
		id bigserial primary key,
		owner_id bigint not null references users(id),
		name text not null,
		summary text,
		tags text[],
		harness text,
		forked_from text,
		created_at timestamptz default now(),
		unique(owner_id, name)
	)`,
	`create table if not exists stack_versions (
		id bigserial primary key,
		stack_id bigint not null references stacks(id),
		version int not null,
		git_tag text,
		manifest jsonb,
		scan_report jsonb,
		changelog text,
		published_at timestamptz default now(),
		unique(stack_id, version)
	)`,
	`alter table users add column if not exists github_id bigint unique`,
	`create table if not exists sessions (
		id bigserial primary key,
		user_id bigint references users(id),
		token_hash text unique not null,
		created_at timestamptz default now(),
		last_used_at timestamptz,
		expires_at timestamptz not null
	)`,
	`alter table stack_versions add column if not exists trust_tier text not null default 'unreviewed'`,
	`alter table sessions add column if not exists purpose text not null default 'cli'`,
	`do $$
	begin
		if not exists (
			select 1 from pg_constraint
			where conname = 'sessions_purpose_check' and conrelid = 'sessions'::regclass
		) then
			alter table sessions add constraint sessions_purpose_check
				check (purpose in ('cli', 'web'));
		end if;
	end
	$$`,
	`create table if not exists follows (
		user_id bigint not null references users(id) on delete cascade,
		stack_id bigint not null references stacks(id) on delete cascade,
		last_seen_version_id bigint references stack_versions(id),
		created_at timestamptz not null default now(),
		primary key (user_id, stack_id)
	)`,
	`create index if not exists follows_stack_id_idx on follows(stack_id)`,
	`create table if not exists events (
		id bigserial primary key,
		type text not null constraint events_type_check check (type = 'stack_published'),
		stack_version_id bigint not null references stack_versions(id),
		created_at timestamptz not null default now(),
		unique(type, stack_version_id)
	)`,
	`create index if not exists events_stack_version_id_idx on events(stack_version_id)`,
	`create table if not exists web_grants (
		token_hash text primary key,
		user_id bigint not null references users(id) on delete cascade,
		handoff_challenge text not null,
		expires_at timestamptz not null,
		created_at timestamptz not null default now()
	)`,
	`create index if not exists web_grants_expires_at_idx on web_grants(expires_at)`,
	`create table if not exists trial_feedback (
		user_id bigint not null references users(id) on delete cascade,
		stack_version_id bigint not null references stack_versions(id) on delete cascade,
		verdict text not null constraint trial_feedback_verdict_check
			check (verdict in ('keep', 'keep_with_notes', 'revert')),
		created_at timestamptz not null default now(),
		updated_at timestamptz not null default now(),
		primary key (user_id, stack_version_id)
	)`,
}

func Migrate(ctx context.Context, db migrationDB) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for _, migration := range migrations {
		if _, err := tx.Exec(ctx, migration); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
