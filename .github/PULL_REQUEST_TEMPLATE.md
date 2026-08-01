## What does this change?

<!-- Brief description. If this is a new rule, link the discussion issue. -->

## Checklist

- [ ] `go build ./...` passes
- [ ] `go vet ./...` passes
- [ ] `go test -race -count=1 ./...` passes
- [ ] `gofmt -l .` prints nothing
- [ ] No rule matches `testdata/healthy.json`
- [ ] If this is a new rule: both a `_match.json` and a realistic `_nomatch.json` (near-miss)
      fixture are included
- [ ] If this is a new rule: it's registered in `internal/rules/rule.go` and pinned in
      `TestRunAllPriority`
- [ ] User-facing strings use `ctx.L(english, french)` — not hardcoded in one language
