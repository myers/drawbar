# Auto step IDs misalign after composite expansion, k8s label sanitize can produce invalid values, and base64 line-wrap corrupts CI registry auth

**Status: fixed 2026-05-08.** Surfaced 2026-05-06 via `/ultrareview` run on PR #1.

Follow-up beyond the original three findings: review of the fix flagged
that `cfg.RunID` (from the forge runner protocol) was reaching k8s job
labels unsanitized in `pkg/k8s/builder.go`. Same fix family as Finding B
— defense-in-depth against a malformed/spoofed `run_id` failing job
creation. `sanitizeLabelValue` was lifted into the new exported
`k8s.SanitizeLabelValue` so both `pkg/snapshot` and `pkg/k8s` can share
it.
Three small independent findings — the "leftovers" group from the
ultrareview triage. Each is genuinely small; together they're one
plucking-them-off session.

## Finding A — auto step IDs use `len(steps)` and drift when an earlier action emitted multiple specs

**Location:** `cmd/controller/main.go:595-599` (or thereabouts —
inside the workflow-spec construction loop).

**The bug.** When a workflow step has no `id:` set, the controller
auto-derives one:

```go
stepID := step.ID
if stepID == "" {
    stepID = fmt.Sprintf("step-%d", len(steps))
}
```

The generated ID uses the count of *already-appended* specs. But
composite actions (and some other action types) can expand a single
workflow step into multiple `StepSpec`s. So if a workflow has:

```yaml
steps:
  - uses: some/composite-action@v1   # expands to 3 StepSpecs
  - run: echo hello                  # auto ID
```

The first step appends 3 specs; the second step starts with
`len(steps) == 3`, so its auto ID becomes `step-3`. The user
expectation (and what GitHub does) is that this would be `step-1`
because it's the second item in the workflow's `steps:` array.

**Concrete consequence.** `${{ steps.step-1.outputs.foo }}`
references in subsequent steps don't resolve because the step the
user thinks of as "step-1" is actually `step-3` in drawbar's view.
Cross-step references break silently.

**Fix sketch.** Track the *workflow* step index separately from the
output spec count:

```go
for workflowIdx, step := range parsed.Steps {
    stepID := step.ID
    if stepID == "" {
        stepID = fmt.Sprintf("step-%d", workflowIdx)
    }
    // ... build specs from step, append all of them ...
}
```

Then composite-action expansions inside the same workflow step share
the parent's ID prefix (`step-N`, `step-N-1`, `step-N-2`) which is
already the established pattern in the same file
(`asID := fmt.Sprintf("%s-%d", stepID, j)`).

## Finding B — `sanitizeLabelValue` can produce k8s-invalid label values

**Location:** `pkg/snapshot/snapshot.go:295-308`.

**The bug.** `sanitizeLabelValue` truncates to 63 chars then replaces
invalid chars with `-`:

```go
func sanitizeLabelValue(s string) string {
    if len(s) > 63 {
        s = s[:63]
    }
    var b []byte
    for _, c := range []byte(s) {
        if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
            b = append(b, c)
        } else {
            b = append(b, '-')
        }
    }
    return string(b)
}
```

Kubernetes label values require:
`(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?` — they MUST start and
end with an alphanumeric, except for the empty string. The current
function happily produces `-foo-bar-` or `_foo` for inputs like
`:foo:bar:` or `_foo`, both of which the kube API server rejects
with `invalid label value`.

**Concrete consequence.** Snapshot creation crashes for branch /
ref names like `feature/foo:bar` or `_dev`, which are valid git
refs but produce invalid k8s labels. Drawbar's snapshot path is
disabled by default, but anyone enabling it on a branch like
`feat/test:wip` hits this.

**Fix sketch.** Trim non-alphanumerics from both ends after sanitize,
then re-truncate to 63 if needed:

```go
func sanitizeLabelValue(s string) string {
    if len(s) > 63 {
        s = s[:63]
    }
    var b []byte
    for _, c := range []byte(s) {
        if isLabelChar(c) {
            b = append(b, c)
        } else {
            b = append(b, '-')
        }
    }
    // Trim leading/trailing non-alphanumerics until we hit one or run out.
    out := strings.TrimFunc(string(b), func(r rune) bool {
        return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
    })
    if len(out) > 63 {
        out = out[:63]
        out = strings.TrimRightFunc(out, ...)  // again, in case truncate cut into trailing alphanumerics
    }
    return out
}
```

Edge case: after trimming, the empty string is allowed. If the input
was *all* non-alphanumeric, return empty.

## Finding C — base64 line-wrap corrupts registry auth JSON in CI workflows

**Location:** `.gitea/workflows/build.yml:41`,
`.woodpecker/build.yaml:41-43, 78-80`,
`actions/build-push/action.yml:62`.

**The bug.** The registry auth header is generated with:

```bash
echo -n "${USER}:${TOKEN}" | base64
```

GNU `base64` (Linux default) wraps at 76 chars by default unless
`-w 0` is passed. macOS `base64` wraps at 76 unless `-b 0` is
passed. Either way, a long auth header (e.g., a GHCR PAT plus a
long username) produces a multi-line string. When that gets
embedded in JSON or in a Docker config, the embedded `\n` corrupts
the auth header and Docker login fails or silently authenticates
wrong.

**Concrete consequence.** CI builds against forks or under specific
auth configurations can fail intermittently or silently push to the
wrong registry. The github workflow `.github/workflows/build.yml`
was NOT flagged because it uses `docker/login-action@v3`, which
handles encoding correctly. The vulnerability is in the
gitea / woodpecker / custom-action paths.

**Fix sketch.** Add `-w 0` (or `tr -d '\n'`) at every base64
invocation in those three files:

```bash
echo -n "${USER}:${TOKEN}" | base64 -w 0
```

`-w 0` is portable to GNU base64 (Linux). For broader portability:

```bash
echo -n "${USER}:${TOKEN}" | base64 | tr -d '\n'
```

Use the `tr -d '\n'` form since drawbar runner images may be
minimal Alpine variants and `base64` flag support varies.

## Why these belong together

Three small unrelated bugs in three unrelated subsystems. Each is a
fast fix with a focused test. Bundling them avoids opening three
separate trivial PRs. None of them depends on the others.

## Test plan sketch

- A: a unit test for the workflow→steps loop in
  `cmd/controller/main.go` with a workflow containing one composite
  action (3 expansions) followed by a no-id `run:` step. Assert the
  `run:` step's `stepID` is `step-1`, not `step-3`.
- B: extend snapshot tests with a table of inputs:
  `_foo`, `foo:bar`, `foo`, `:::`, `123-foo` → assert outputs are
  all k8s-valid (start and end alphanumeric, ≤63 chars).
- C: a shell-test that runs `bash <build.yml-snippet>` with a long
  username and asserts the produced auth string contains no `\n`.

## Source

Filed via `/ultrareview` run on PR #1, 2026-05-06.

## Related

- The github workflow is correct (uses official login action).
  Finding C is gitea/woodpecker/custom-action only.
- Bug 016 area: finding A's step-ID drift can confuse the
  reporter's per-step accounting; the bug 016 fix doesn't address
  ID *generation*, only ID *transmission*.
