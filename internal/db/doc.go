// Package db owns the Postgres schema and its access layer.
//
//   - migrations/: goose SQL migrations, embedded and applied by `clipmux serve`
//     on every boot (Migrate is idempotent).
//   - queries/: SQL for sqlc; generated code lives in sqlc/ and is committed.
//     Regenerate with `make sqlc`; CI fails if it is stale.
//   - dbtest/: a fresh, fully migrated schema per test on real Postgres.
//
// Every row carries project_id, IDs come from internal/ids, and states are
// CHECK-constrained so no code path can write an invalid one.
package db
