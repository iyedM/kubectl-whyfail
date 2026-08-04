# Contributing to kubectl-why-fail

Thanks for considering a contribution. This project stays small and reliable on purpose —
here's what that means in practice.

## Adding a new rule

The full walkthrough (with a code example) lives in the
[README's "How to add a rule" section](README.md#how-to-add-a-rule). Read that first.

The v1 scope is intentionally closed at 10 rules to keep every one of them 100% reliable.
**Open an issue to discuss a new rule before opening a PR for it.** This is a scope decision,
not a code review — we want to agree on the failure mode and its priority before you spend
time writing the rule, tests, and fixtures.

## Before you open a PR

Run the full verification suite locally:

```bash
go build ./...
go vet ./...
go test -race -count=1 ./...
gofmt -l .
```

`gofmt -l .` must print nothing. `go test` must pass with no skips.

## The two invariants that must never break

These are enforced by the test suite, and a PR that breaks either will be rejected:

1. **No rule may ever match `testdata/healthy.json`.** A confident wrong answer is worse
   than no answer at all.
2. **The LLM fallback is never called once a rule has matched.** This is the whole point of
   the collector → rules → LLM pipeline; guarded by an integration test in
   [`cmd/whyfail/main_test.go`](cmd/whyfail/main_test.go).

If your change touches `internal/rules` or `cmd/whyfail`, make sure these still hold.

## Language consistency

User-facing text always goes through `ctx.L(english, french)` — never hardcode one language,
and never mix English and French within the same string. If you're adding a rule, write both
variants; if you're only comfortable in one language, say so in the PR and someone can help
with the translation.

## Workflow

1. Fork the repo, create a branch off `main`.
2. Make your change, with tests. For a new rule, that means a `_match.json` fixture (a
   realistic failure) and a `_nomatch.json` fixture (a realistic **near miss** — something
   that looks similar but has a different root cause). The near-miss fixture is what actually
   prevents false positives, so don't skip it.
3. Run the verification suite above.
4. Open a PR with a clear description of what changed and why. Link the discussion issue if
   this is a new rule.

## Reporting bugs

Please use the bug report template rather than a blank issue — it asks for the exact command
and pod state, which is usually most of what's needed to reproduce.
