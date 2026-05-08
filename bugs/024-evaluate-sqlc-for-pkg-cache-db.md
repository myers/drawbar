# Evaluate moving `pkg/cache/db.go` to sqlc

**Status: filed (proposal, not a defect).** Surfaced 2026-05-07 during
the bug-019 LIKE-escape fix — adding a hand-rolled `likeEscaper` to
`pkg/cache/db.go` triggered the question: are we slowly building a
home-grown ORM in this package?

## What's there today

`pkg/cache/db.go` is ~200 lines of raw `database/sql` against one
SQLite table (`caches`):

- One `Cache` struct (7 columns).
- Hand-rolled `queryOne` and `scanAll` scan helpers that hard-code the
  column order in two places.
- Eight CRUD/query functions: `OpenDB`, `InsertCache`, `GetCache`,
  `CompleteCache`, `TouchCache`, `DeleteCache`, `FindCache`,
  `FindExpired`, `FindDuplicates`.
- Inline SQL strings as Go raw-string literals.
- A `likeEscaper` Replacer (added in bug 019, finding A) for the
  prefix-match `LIKE` query.

It works. The total surface is small. But every operation we add comes
with: a new SQL string, a new Go signature, a new place where column
order has to match the scan helper, and a new test that verifies the
query parameters were threaded correctly. That's the bottom 5% of an
ORM, written in-tree.

## Why this came up

Bug 019, finding A, fixed a SQL `LIKE` injection by adding a 3-char
escape. There is no "the" Go library for LIKE-escape — it's small
enough that no package publishes one. So at the level of *that single
fix*, "buy instead of build" doesn't apply. But the broader question
— are we drifting toward a homegrown ORM? — does apply, and the answer
is "we're closer than we should be." Mostly because we hand-roll
`queryOne`/`scanAll` and column ordering.

## Proposal

Move `pkg/cache/db.go` to [**sqlc**](https://github.com/sqlc-dev/sqlc):

- Queries live in `.sql` files (`pkg/cache/queries.sql`).
- `sqlc generate` produces typed Go funcs (one per query) and the
  `Cache` struct from the schema.
- We keep raw SQL — no method-call DSL, no runtime reflection, no
  surprise behaviour. The generated code is what we'd have written by
  hand, just consistent.
- `LIKE`-escape is still our problem (sqlc doesn't ship a helper), but
  there's only one prefix-match query, so the Replacer is a 3-line
  helper next to the one caller.

Why sqlc rather than GORM/ent/bun:

- One table, seven columns. We don't need migrations, hooks, or
  associations.
- We already write good SQL by hand — sqlc keeps that habit.
- Compile-time-checked queries (sqlc parses the SQL against the
  schema) catch column-ordering and type drift, which is the actual
  failure mode we want to eliminate.
- No runtime dependency added: sqlc is a build-time codegen tool.
- SQLite support is mature in sqlc.

Alternatives considered: GORM is overkill for one table; ent imposes
its schema DSL; bun is closer to sqlc but heavier and adds a runtime
dep.

## Scope

- Add `sqlc.yaml`.
- Move the schema constant from `db.go` into a `schema.sql`.
- Translate the eight functions into named SQL queries with sqlc
  annotations (`-- name: GetCache :one` etc.).
- Wire `make gen` into the build (or a separate `make sqlc` if we
  want it gated).
- Update callers in `pkg/cache/handler.go` and the GC paths to use
  the generated funcs.
- Migrate `handler_test.go::TestCacheDB_CRUD` and `db_test.go` (added
  by bug 019, finding A) to the generated API. Should be mechanical.
- Keep `likeEscaper` next to the prefix-match call site.

Not in scope: changing the schema, adding migrations tooling, or
touching `pkg/cache/storage.go` / `pkg/cache/handler.go` HTTP routing.

## Open questions

- Do we want sqlc as a build-time prereq (developer must `brew install
  sqlc`), or vendored as a Go tool dependency via `tools.go`? Vendoring
  is friendlier for fresh checkouts.
- The current `db.SetMaxOpenConns(1)` for SQLite WAL: does the sqlc
  template pattern still let us own the `*sql.DB` lifecycle and tune
  the pool? (It does — sqlc generates a `Querier` interface plus a
  `Queries` struct that wraps a `*sql.DB`. We keep `OpenDB`.)

## Why this is filed separately from bug 019

Bug 019 is three independent correctness fixes. Mixing in a database-
layer rewrite would: bloat the diff, blur the bisect history, and put
"replace `database/sql` with sqlc" into a commit titled "fix LIKE
escape." This proposal stands on its own merits and should be evaluated
on its own.

## Source

Surfaced during conversational review of bug 019 finding A,
2026-05-07.
