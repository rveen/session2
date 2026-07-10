# Changelog

All notable changes to this package are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [1.1.0] - 2026-07-10

Storage is now bounded and self-trimming, and the global manager is safe to swap
while requests are in flight. All changes are source-compatible with v1.0.0.

Note for upgraders: `Add` no longer grows the store without bound once
`MaxSessions` is set. Leaving it at its `0` default preserves v1.0.0 behaviour
exactly.

### Fixed

- **`Init` no longer races with in-flight requests.** `global` was a plain
  package variable that `Init` and `Reinit` reassigned while `Get`, `Add`,
  `Remove`, `Close` and `Len` read it without synchronisation. It is now an
  `atomic.Pointer[manager]`, swapped in one step, with the displaced manager's
  cleaner goroutine stopped afterwards. `Init` still discards every stored
  session, so it remains a startup-only call; this is now stated in its doc
  comment.

- **The session map releases memory after a traffic spike.** Deleting from a Go
  map leaves its bucket array allocated, so a burst that filled the store raised
  the process floor for the lifetime of the manager — roughly 3.5 MB retained at
  100 000 sessions and 56 MB at one million, even after every session had
  expired. The cleaner now rebuilds the map once occupancy falls below a quarter
  of its high-water mark (and only above a 1024-entry floor, so small stores are
  never rebuilt).

### Added

- **`Options.MaxSessions`** caps the number of stored sessions. `0`, the
  default, means unlimited and preserves the previous behaviour.

- **`SetMaxSessions(n)`** changes the cap on the live manager, for callers that
  apply configuration after `Init`. Lowering the cap does not evict immediately;
  the store is brought down to the new cap on the next `Add`.

- **LRU eviction at the cap.** When the store is full, `Add` drops the least
  recently accessed sessions to make room, so adding a session never fails and a
  full store never locks out new callers. Eviction removes a batch per scan
  rather than a single entry, which amortises the O(n) scan across many inserts
  once the store sits at its cap.

  Callers that previously guarded `Add` with their own `Len() > max` check
  should drop it: that pattern is a check-then-act race, since the check and the
  insert take the manager lock separately. The cap is now enforced inside `Add`
  under a single write lock.

- Tests for eviction at the cap, `SetMaxSessions` on a live manager, map
  compaction, the small-map compaction floor, and `Init` swapping under
  concurrent `Get`/`Add`/`Len` (meaningful under `-race`).

## [1.0.0]

- Initial release: a simplified extraction of the `session` package.
