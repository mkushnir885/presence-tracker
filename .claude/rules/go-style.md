---
description: Go-specific conventions for this project
globs: ["go/**/*.go", "go/go.mod", "go/go.sum"]
---

# Go style rules

## Language and stdlib

- Go 1.23+. Use modern stdlib features (`slices`, `maps`, `log/slog`,
  `errors.Join`, range-over-func where genuinely clearer).
- Logging: `log/slog` exclusively. No `fmt.Println` in non-CLI code.
  CLI output to stdout is fine; structured logs go to stderr via slog.
- Errors: always wrap with `%w`. Use `errors.Is` / `errors.As` at
  boundaries. Define sentinel errors as package-level
  `ErrFoo = errors.New("...")`.
- Contexts: first arg, named `ctx`. Never stored in structs. Every
  blocking call must be cancellable.
- Concurrency: goroutines owned by the function that starts them; always
  have a clear shutdown path. Prefer `errgroup.Group` over ad-hoc
  `sync.WaitGroup` when goroutines can error.
- No generics unless they meaningfully reduce duplication.
- No panics in library code. `main` may panic on config errors.

## Project-specific patterns

- `internal/` for everything. Nothing in this project is a public library.
- Every exported package has a `doc.go` with a package comment.
- Interfaces defined on the consumer side, kept small. Adapter packages
  don't define the interface — the consumer package does.
  - `go/src/internal/providers/provider.go` defines `Provider`
  - `go/src/internal/messengers/messenger.go` defines `Messenger`
  - `go/src/internal/challenges/challenge.go` defines `ChallengeType` and `Poller`
- Adapter packages (`go/src/internal/providers/bbb/`, `go/src/internal/messengers/telegram/`,
  etc.) depend only on the interface package, never on each other.
- Package names: short, lowercase, no underscores. `eventstore` not
  `event_store`.

## Comments

- Write comments only where the logic is non-obvious or the intent cannot
  be inferred from the names alone. Self-documenting code needs no comment.
- Never restate what the code does in a comment ("increment counter",
  "return error"). Say _why_ when the why is not obvious.
- Every exported identifier in `doc.go` gets a package comment; other
  exported identifiers get doc comments. Unexported identifiers: comment
  only if the behavior would surprise a reader.

## Long-running processes

- Adapters that own long-running work (provider pollers, messenger
  bots, audio relay) expose a `Start(ctx) error` / `Stop(ctx) error`
  pair. Events emitted via a channel returned from `Subscribe` or similar.
- On `ctx.Done()`, the goroutine must drain, close its channel, and
  return within a short bound (a few seconds). The session coordinator
  relies on this for clean shutdown.

## Tests

- Table-driven by default. Test files next to the code (`foo_test.go` in
  the same package); use `_test` package suffix only when you specifically
  want black-box testing.
- Fixtures live under `test/fixtures/`. Golden files use `-update` flag
  convention (`go test -update`).
- For every adapter package, include a test that runs against the mock
  provider/messenger so the behavior contract is exercised.

## Tooling

- Format with `gofmt`; lint with `golangci-lint run`.
- Don't disable lints inline without a comment explaining why.
