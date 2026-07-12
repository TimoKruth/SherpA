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
