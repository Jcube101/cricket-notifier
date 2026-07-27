# Learnings

Notes-to-self from building this. It's a learning project, so this file is as
much the point as the code.

## Go concepts this project uses

Each of these appears in the codebase — grep for them to see a real example in
context rather than a textbook one.

- **`if err != nil` error handling.** Go has no exceptions. Functions that can
  fail return an `error` as their last value and you check it right after the
  call. It's everywhere in `cricket.go` and `main.go`.

- **Multiple return values.** Functions return more than one thing directly.
  `fetchMatchScore` returns `(ScoreState, int, error)` — the score, the
  remaining quota, and an error — with no wrapper struct.

- **`defer`.** Schedules a call to run when the surrounding function returns,
  used to guarantee cleanup. `defer resp.Body.Close()` right after opening an
  HTTP response body means it always gets closed, however the function exits.

- **`fmt.Errorf` with `%w`.** Wraps a lower-level error with context while
  preserving the original for inspection: `fmt.Errorf("decode leanback: %w",
  err)`. The `%w` verb is what makes it a *wrap* rather than a flat string.

- **Goroutines.** Lightweight concurrent functions started with the `go`
  keyword. `main` launches the discovery and watch loops as two goroutines that
  run independently.

- **Channels.** Typed pipes for communicating between goroutines. Here they show
  up indirectly: `time.Ticker` delivers ticks on a channel (`ticker.C`), and
  `ctx.Done()` is a channel that closes on shutdown. A `select` waits on both at
  once.

- **`time.Ticker`.** Fires on a channel at a fixed interval — the heartbeat of
  each polling loop. Remember to `defer ticker.Stop()`.

- **Context cancellation.** A `context.Context` threads a cancellation signal
  through the program. `signal.NotifyContext` produces one that's cancelled on
  SIGTERM/SIGINT; the loops watch `ctx.Done()` to know when to exit, and it's
  passed into each HTTP request so an in-flight call aborts on shutdown.

- **`os/signal`.** How a Go program catches OS signals. `signal.NotifyContext`
  ties SIGTERM (what `systemctl stop` sends) and SIGINT (Ctrl-C) to context
  cancellation, which is what makes graceful shutdown work.

- **`log/slog`.** Go's structured logger (standard library). Calls like
  `slog.Info("now watching match", "matchId", id)` produce key/value log lines
  that land in the systemd journal.

- **`encoding/json`.** Decodes the API's JSON into structs. Struct tags
  (`` `json:"matchId"` ``) map JSON field names to Go fields. The API uses
  different casing in different endpoints (`matchId` vs `matchid`), which the
  tags quietly absorb.

- **`net/http`.** The HTTP client. A reused `*http.Client` with a timeout makes
  the API calls; `http.NewRequestWithContext` is what lets the context cancel a
  request.

- **`sync.Mutex` / `sync.WaitGroup`.** A mutex guards the state shared between
  the two loops (the active match id and last snapshot); a wait group lets
  `main` block until both loops have actually finished before exiting.

## The Cricbuzz endpoint situation

The original plan assumed free, reverse-engineered Cricbuzz JSON endpoints, the
kind a lot of older hobby projects use. By the time this was built, those were
**dead**: the old `apiserver.cricbuzz.com` host no longer resolves,
`www.cricbuzz.com` is now a Next.js app whose old JSON paths 404, and
ESPNcricinfo's hidden API blocks unknown clients.

The pivot was to the **same Cricbuzz data via RapidAPI** — which keeps the data
shape the plan assumed but moves it behind an API key and a hard 200
requests/month quota. Lesson: "widely used free endpoint" has a shelf life;
verify the data source is actually reachable *before* modelling structs against
it.

## The activity-log crash-loop (observability that took the service down)

Adding a persistent activity log looked harmless and nearly bricked the service.
The logger created its `logs/` directory on startup and `main.go` treated a
failure to open the log as fatal (`os.Exit(1)`). But the systemd unit sandboxes
the project directory read-only (`ProtectSystem=strict`, `ProtectHome=read-only`,
`ReadOnlyPaths=<project>`), so `mkdir logs` failed with *read-only file system*,
the process exited 1 within ~11 ms, and systemd restarted it — **~6900 times**,
saturating a shared, size-capped journal on the Pi and crowding out every other
service's logs.

Three lessons, each now baked into the code/unit:

- **Observability must never be a hard dependency.** A logging failure should
  degrade (warn + disabled no-op logger), never stop the program starting. If a
  feature exists purely to help you see what's happening, it must not be able to
  stop the thing you're watching.
- **A sandbox changes what "can write here" means.** The code ran fine when
  built and run by hand — the read-only constraint only exists under the systemd
  unit. A write the developer never sees fail can be fatal in production. The fix
  was `ReadWritePaths=<project>/logs` (and the dir must pre-exist; systemd
  bind-mounts it).
- **Restart throttling only works if the window fits the backoff.** The unit had
  the default `StartLimitBurst=5` but a 10s `StartLimitIntervalSec`, while
  `RestartSec=30s`. At most one restart lands per 10s window, so the burst
  counter never fills and the limiter *never trips* — an infinite loop with a
  throttle that does nothing. The window must exceed `RestartSec × Burst`
  (now 300s), so a genuine crash-loop fails into a stopped state after 5 tries.

Bonus corollary discovered while debugging: an unclean shutdown left four
zero-length objects in `.git` (the loose objects mid-write when power was lost),
which `git fsck` flagged as *object file … is empty*. Because the remote had the
commit intact, recovery was: back up `.git`, `find .git/objects -type f -empty
-delete`, `git fetch origin`. The ext4 error counter (`/sys/fs/ext4/…/errors_count`)
read 0, confirming the filesystem itself was fine — only in-flight writes were lost.

## Why the two loops are mutually exclusive

This was a budget decision, not a technical one. There's nothing stopping the
discovery and watch loops from running concurrently — but with only 200 API
requests a month, every redundant call matters. So discovery goes silent while a
match is being watched, and the watch loop goes silent while idle. Each request
is spent only when it can actually tell us something new.

## "Delay" means two different things (found while building the test suite)

Capturing real fixtures for the test suite (see TESTING.md) turned up something
no hand-written fixture would have: `/matches/v1/live` and
`/mcenter/v1/{id}/leanback` disagreed about the same match's `state` at the same
instant — one said `"In Progress"`, the other said `"Delay"`. Digging into why
led to the real problem: Cricbuzz reuses the string `"Delay"` for two unrelated
situations — rain before a ball has been bowled, and a rain/bad-light stoppage
mid-match. `isPreMatch` didn't recognise `"Delay"` at all, so a pre-match delay
that later cleared up looked identical to any other "was waiting, now playing"
transition and fired **"🏏 Match started"** while nobody was playing.

The tempting fix — add `"Delay"` to `isPreMatch` — just traded one false
positive for another: the captured Day 3 Test fixture sits in `"Delay"` mid-match,
and a `Delay` → `In Progress` transition there would announce the match starting
again, potentially several times during a single rain-hit Test.

The actual fix doesn't live in the state string at all: gate the start message on
the innings id going `0` → non-zero. A genuine pre-match delay has no innings yet
under any circumstance; a mid-match delay always does. Widening `isPreMatch` to
name the states honestly (`delay`, `rain`, `wet outfield`, `inspection`) still
matters for accuracy, but the innings guard is what actually makes both cases
behave correctly. Lesson: when an upstream API reuses one value for two
semantically different situations, look for a second signal that's absent in
exactly one of them, rather than trying to special-case the value itself.
