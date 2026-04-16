# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Run tests with race detection and coverage (matches CI)
go test -race -coverprofile=coverage.txt -covermode=atomic

# Run a single test
go test -run TestName -v

# Run all tests verbosely
go test -v ./...
```

## Architecture

A fork of `github.com/icza/session` — a cookie-based HTTP session management library. The module path is `github.com/trukeio/session2`. The entire library lives in a single file, `session.go`, with no external dependencies.

### Two types

**`Session`** — stores attributes and metadata for one HTTP session. All attribute access is guarded by an `RWMutex`. The single `attrs` map is mutable via `SetAttr`/`Attr`/`Attrs`. Pass `nil` to `SetAttr` to delete a key.

**`Manager`** — combines in-memory storage, HTTP cookie I/O, and background expiry in one struct. Create with `NewManager(Options{})`. Call `Close()` when done to stop the background goroutine. Key methods: `Get(r)`, `Add(sess, w)`, `Remove(sess, w)`, `Len()`.

### Session cleanup

The `Manager` runs a single background goroutine (`cleaner`) that ticks at `Options.CleanInterval` (default 10 s). Each tick acquires a write lock and deletes sessions where `time.Since(accessed) > timeout`. Session timeout is per-session (set via `SessOptions.Timeout`, default 30 min).

### Session ID generation

IDs are 18 `crypto/rand` bytes, base64 URL-encoded → 24-character strings.

### Cookies

`Manager.Add` sets `HttpOnly: true` and `Secure: !AllowHTTP`. Pass `AllowHTTP: true` in `Options` for local HTTP testing. Default cookie name is `"sessid"`, path `"/"`, max-age 30 days.

### Global convenience API

A package-level `Global *Manager` is initialised with `AllowHTTP: true` and a 30-minute clean interval. Package-level `Get`, `Add`, `Remove`, `Close` delegate to it. Tests that use the global must reset it: `Global.Close(); Global = NewManager(...)`.

## Tests

Tests use inline `eq`/`neq` helpers (no external assertion library). Time-dependent tests (`TestManagerCleaner`) configure short intervals (10–80 ms) to keep the suite fast. `TestGlobal` runs a real `httptest.Server` with an `http.CookieJar` to exercise the full request/response cycle.
