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

## Why the two loops are mutually exclusive

This was a budget decision, not a technical one. There's nothing stopping the
discovery and watch loops from running concurrently — but with only 200 API
requests a month, every redundant call matters. So discovery goes silent while a
match is being watched, and the watch loop goes silent while idle. Each request
is spent only when it can actually tell us something new.
