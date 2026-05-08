# Cache layer correctness: LIKE wildcards in user keys, mod-255 bucket skew, ActionDir collision

**Status: filed.** Surfaced 2026-05-06 via `/ultrareview` run on PR #1.
Three findings in the cache-key safety domain — `pkg/cache/db.go`,
`pkg/cache/storage.go`, `pkg/actions/resolve.go`. All "the cache says
the right thing but the keying is wrong." One mental model: cache-key
safety. One session.

## Finding A — `FindCache` LIKE without escaping `%` and `_`

**Location:** `pkg/cache/db.go:70-75`.

**The bug.** The prefix-match query uses

```go
`SELECT ... WHERE key LIKE ? AND version = ? AND complete = 1
 ORDER BY created_at DESC LIMIT 1`, prefix+"%", version
```

without escaping SQL LIKE wildcards `%` and `_` in the user-supplied
`prefix`. Cache keys come from workflows, not from drawbar — so a
workflow can include `%` or `_` characters in a `restore-keys` entry
and accidentally match more than intended.

Concrete shape: `restore-keys: ["foo%"]` matches any key starting with
`foo` followed by anything, but ALSO any key that happens to contain
that literal `%`. More dangerous: `restore-keys: ["a%b"]` becomes
`LIKE 'a%b%'` — matches `axyzbANYTHING`, including caches from
unrelated builds.

The exact-match query (`WHERE key = ?`) is fine — `=` doesn't
interpret wildcards.

**Fix sketch.** Escape `%`, `_`, and `\` in `prefix` before
concatenating, and add `ESCAPE '\'` to the LIKE clause:

```go
escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
c, err = queryOne(db,
    `SELECT ... FROM caches
     WHERE key LIKE ? ESCAPE '\' AND version = ? AND complete = 1
     ORDER BY created_at DESC LIMIT 1`, escaped+"%", version)
```

## Finding B — `Storage.filename` uses `id%0xff` (255) instead of `id%0x100` (256)

**Location:** `pkg/cache/storage.go:105-107`.

**The bug.**

```go
func (s *Storage) filename(id uint64) string {
    return filepath.Join(s.rootDir, fmt.Sprintf("%02x", id%0xff), fmt.Sprint(id))
}
```

`0xff` is 255, so this yields buckets `00` through `fe`. Bucket `ff`
is never created. The format string `%02x` is sized for 256 buckets
(two hex digits, max `ff`), so the *intent* is clearly mod 256 — the
literal is a typo.

Cache distribution is uneven: bucket `00` gets ids `0, 255, 510, ...`
on top of the ids it would normally get under mod 256, while bucket
`ff` is empty. Not data-loss-shaped — every cache file still has a
unique path because the full id is part of the filename — just an
even-distribution defect.

**Fix sketch.** `id%0xff` → `id%0x100`. **However:** existing caches
on disk that landed in bucket `00` because of the old modulo will not
be findable under the new modulo. For early alpha with no in-flight
users to protect this is acceptable, but worth noting in the commit
message. If anyone has a populated cache, they need to either
re-warm or run a one-shot rebucket script.

To be precise about which ids drift: under the old `id%0xff`, bucket
`00` held the natural-mod-256 ids `0, 256, 512, ...` *plus* the
overflow set `255, 510, 765, ...` (every `255 + 255k`). Under the new
`id%0x100`, the natural set still hashes to `00` and remains findable;
the overflow set hashes to `ff, fe, fd, ...` and is unfindable until
re-warmed. So the unfindable subset is specifically `{255 + 255k}`,
not all of bucket `00`.

## Finding C — `ActionDir` collision: refs differing only in `.` vs `-` map to same dir

**Location:** `pkg/actions/resolve.go:110-121`.

**The bug.** `ActionDir` sanitizes the ref by mapping any character
outside `[a-zA-Z0-9_-]` to `-`:

```go
func (a *ActionRef) ActionDir() string {
    name := fmt.Sprintf("%s-%s-%s", a.Org, a.Repo, a.Ref)
    name = strings.Map(func(r rune) rune {
        if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
            return r
        }
        return '-'
    }, name)
    return name
}
```

Note `.` is NOT in the allowed set, but `-` is. So `actions/cache@v4.0.0`
and `actions/cache@v4-0-0` both produce dir `actions-cache-v4-0-0`.
Two action versions that differ only in `.` vs `-` (or any other
non-alphanumeric character) share a cache directory.

**Concrete consequence.** Workflow A pins `actions/cache@v4.0.0`,
workflow B pins `actions/cache@v4-0-0` (less common but valid as a
git tag), and they share the same on-disk action source — last writer
wins. Cache poisoning shaped, though the surface is narrow because
git tags rarely use both forms in practice.

A bigger real-world hit: `actions/cache@1.0` vs
`actions/cache@1-0` would collide; refs containing `/` (path-like
sub-actions, e.g. `foo/bar/sub@v1`) collapse all path separators to
`-` and could collide with refs containing literal dashes.

**Fix sketch.** Either:
1. Allow `.` in the allowed set (most robust — matches what `git`
   already permits in tag/branch names).
2. Use a different separator for sanitized characters (e.g., `_`) so
   `.` collisions don't conflate with literal `-`.
3. Hash the original ref alongside the sanitized name:
   `actions-cache-v4-0-0-<sha8(v4.0.0)>`. Bulletproof but ugly.

(1) is the smallest change. (3) is the most robust if collisions are
ever observed in practice.

## Why these belong together

All three live in the cache-keying domain — "we mis-key, so we mis-cache."
Crossing two packages (`pkg/cache/` and `pkg/actions/`) is fine; the
mental model is the same. Diagnostic flow is also shared: trace a
specific workflow input through the keying logic and see where two
different inputs end up in the same place.

## Test plan sketch

- A: a unit test for `FindCache` with a populated cache containing
  keys `foo`, `foobar`, `foo%bar`. Search with restore-keys
  `["foo%"]` — should match only `foo%bar` (literal), not `foobar`.
- B: a unit test for `filename` over a representative range of ids,
  asserting that `0xff..0x1fe` lands in distinct buckets and that
  bucket `ff` is reachable.
- C: a unit test for `ActionDir` over the table:
  - `actions/cache@v4.0.0` → some dir
  - `actions/cache@v4-0-0` → DIFFERENT dir
  - `actions/cache@v4_0_0` → also different
  - All three should map to distinct strings.

## Source

Filed via `/ultrareview` run on PR #1, 2026-05-06.

## Related

- The "three caches" section in CLAUDE.md is the right map for this
  fix: actions-source cache (#C), workspace-snapshot cache
  (untouched), artifact cache (#A, #B).
