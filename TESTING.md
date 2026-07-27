# Testing

The plan for this project's test suite: what exists, what's missing, and the
exact set of tests worth writing. For the day-to-day dev workflow see
[CLAUDE.md](CLAUDE.md); for the design tradeoffs being tested see
[SPEC.md](SPEC.md).

This document is written to be executed. Each test below is a checkbox; tick
them off as they land.

## Ground rules

- **No network, ever.** Every test runs offline. The API budget is 200
  requests/month ([SPEC.md](SPEC.md)) — a test suite that spends it is worse
  than no test suite. HTTP is exercised against `httptest.Server`, never
  RapidAPI, never Telegram.
- **No sleeping on real intervals.** Loop tests drive `discover` / `watch`
  directly, or use millisecond intervals with a cancellable context. The whole
  suite should finish in well under a second.
- **`-race` is not optional.** `controller` is shared by two goroutines and
  guards state with two separate mutexes (`mu`, `firedMu`). The suite must pass
  `go test -race ./...`.
- **Test the documented invariants, not the implementation.** The rules in
  CLAUDE.md ("Things to keep in mind when changing the code") are precisely the
  things that should fail loudly when someone breaks them. Several tests below
  exist only to pin those.

## Where things stand

`notifier_test.go` is the only test file: 7 tests, one per notification event,
driven through a fake sender. It is good work — the tests deliberately nudge
unrelated fields so exactly one event fires per case.

It is also the only file under test.

| File | Statements covered | Gap |
|------|-------------------|-----|
| `notifier.go` | ~85% | happy path per event only; no multi-event, no fallbacks |
| `cricket.go` | **0%** | API decoding, filters, quota header, error type |
| `main.go` | **0%** | the entire state machine and quota guard |
| `activity.go` | **0%** | rotation, and the disabled-logger crash-loop class |
| **total** | **16.0%** | |

The untested 84% is where the risk lives. The notifier is pure functions over a
struct. `main.go`, `cricket.go` and `activity.go` hold the concurrency, the
nil-pointer JSON, the quota state, and a bug that has already taken the service
down once (see [LEARNINGS.md](LEARNINGS.md)).

## What the real capture revealed

Capturing live data (2026-07-28, WI vs PAK) surfaced three things that no
hand-written fixture would have. Two are handled; one is an open question.

**1. The two endpoints disagree about `state`.** For match `152496`, at the same
moment, `/matches/v1/live` reported `"In Progress"` while
`/mcenter/v1/152496/leanback` reported `"Delay"`. Discovery reads the first,
the notifier diffs the second, so they are not interchangeable.

**2. `"Delay"` is in neither allowlist, and it means two different things.**
`isPreMatch` and `isTerminal` ([notifier.go:123](notifier.go#L123)) are closed
lists with a permissive default, so `"Delay"` counts as "playing". A match going
`Preview` → `Delay` (rain before the first ball) therefore fires **"🏏 Match
started"** while nobody is playing.

The catch is that Cricbuzz reuses `"Delay"` for mid-match interruptions too —
our captured fixture is a Day 3 Test sitting in `"Delay"`. So simply adding it
to `isPreMatch` trades one false positive for another: `Delay` → `In Progress`
on day 3 would announce the match starting again, potentially several times in a
rain-hit Test.

**Resolved (see Step 0b):** widen `isPreMatch` *and* gate the start message on
the innings id going `0` → non-zero. The innings guard is what actually fixes
both, since a genuine pre-match delay has no innings yet.

`isWatchable` already handles `"Delay"` correctly, so this was never a budget or
crash bug.

**3. `inningsscores` arrives newest-first.** So the active innings sits at index
0 and a "take the first line" bug would pass against real data. Hence
`leanback_innings_current_not_first.json`.

## Step 0 — the enabling refactor

`controller.client` is a concrete `*CricketClient`, and `apiBaseURL`
([cricket.go:31](cricket.go#L31)) is a package const. Nothing in `cricket.go` or
`main.go` can be tested without hitting RapidAPI.

Fix: give `CricketClient` a `baseURL` field, defaulted in the constructor.

```go
type CricketClient struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

func NewCricketClient(apiKey string) *CricketClient {
	return &CricketClient{
		apiKey:  apiKey,
		baseURL: apiBaseURL,
		http:    &http.Client{Timeout: 20 * time.Second},
	}
}
```

`get` then builds `c.baseURL + path` instead of `apiBaseURL + path`. Tests
construct the client directly (same package, so unexported fields are reachable):

```go
srv := httptest.NewServer(handler)
defer srv.Close()
c := &CricketClient{apiKey: "test-key", baseURL: srv.URL, http: srv.Client()}
```

**Why this and not a `scoreSource` interface.** An interface would stub out the
HTTP layer — but the HTTP layer (header parsing, status handling, JSON
decoding against the real messy payload shape) is exactly the part that has
never been tested. The `baseURL` seam keeps all of it live. It is also three
lines and changes no behaviour.

Two supporting notes for whoever writes the loop tests:

- `controller.activity` must be non-nil. `activityLogger`'s methods have a
  pointer receiver and take `l.mu`, so a nil `*activityLogger` panics. Use
  `newDisabledActivityLogger()` — which is itself the thing Tier 2 tests.
- `controller.notifier` should be `&Notifier{send: fn}` with a capturing `fn`,
  the same pattern `newTestNotifier` already uses.

## Step 0b — two behaviour fixes

Unlike Step 0, these **do** change behaviour. Make them before writing the Tier
3 notifier tests, so those tests are written against the intended behaviour
rather than being rewritten straight afterwards.

Both are decided; do not re-litigate them. Both are small, and neither breaks
any existing test in `notifier_test.go` — verify that first, then extend.

**(a) Match start requires the first innings to begin.** Widen `isPreMatch` to
name the pre-first-ball states honestly, and add the innings guard that does the
real work:

```go
func isPreMatch(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", "preview", "upcoming", "scheduled", "toss",
		"delay", "rain", "wet outfield", "inspection":
		return true
	}
	return false
}
```

```go
// checkAndNotify, event 1
if isPreMatch(prev.State) && !isPreMatch(curr.State) && !isTerminal(curr.State) &&
	prev.InningsID == 0 && curr.InningsID != 0 {
	n.notify(fmt.Sprintf("🏏 %s — Match started", curr.Title()))
}
```

Do **not** add mid-match break states (`stumps`, `lunch`, `tea`, `drinks`,
`innings break`) to `isPreMatch`. They are not pre-match, and the innings guard
already stops them mattering.

Required truth table — turn it into a table test:

| prev | curr | expected |
|------|------|----------|
| `Preview` (inn 0) | `Delay` (inn 0) | silent |
| `Delay` (inn 0) | `In Progress` (inn 1) | **"Match started"** |
| `Delay` (inn 3) | `In Progress` (inn 3) | silent |
| `Preview` (inn 0) | `In Progress` (inn 1) | **"Match started"** |
| `Innings Break` (inn 2) | `In Progress` (inn 3) | silent (innings-change path) |

**(b) Batter milestones only for batters we have a baseline for.** Today
`runsForBatter` returns `0` for an unknown batter, so anyone appearing at the
crease already past a milestone announces it — a retired-hurt batter returning
on 60 re-announces their fifty. Return a found flag and skip unknowns:

```go
func (s ScoreState) runsForBatter(name string) (int, bool) {
	switch name {
	case s.Striker.Name:
		return s.Striker.Runs, true
	case s.NonStriker.Name:
		return s.NonStriker.Runs, true
	}
	return 0, false
}
```

```go
// checkAndNotify, event 5
prevRuns, known := prev.runsForBatter(b.Name)
if !known {
	continue // new or returning batter: no baseline, so no milestone
}
```

A batter who comes in at 0 and reaches 50 while at the crease is present in the
previous snapshot by then, so the normal case is unaffected. Both branches need
a test: known batter crossing 50 fires; unknown batter on 60 stays silent.

## File layout

| File | Status | Contents |
|------|--------|----------|
| `notifier_test.go` | exists — extend | event diffing, formatting, helpers |
| `cricket_test.go` | **new** | API client, JSON → `ScoreState`, filters |
| `controller_test.go` | **new** | `discover` / `watch` state machine, quota guard, env |
| `activity_test.go` | **new** | rotation, disabled logger, line formatting |
| `testdata/*.json` | **new** | captured API response fixtures |

---

## Tier 1 — the API boundary and the state machine

Highest value per line. Nothing here is covered today.

### `cricket_test.go`

**`toScoreState` — JSON to snapshot.** Drive from fixtures in `testdata/`.

- [x] `leanback_test_day3.json` maps onto every `ScoreState` field. The verified
      expected value is: `Format "TEST"`, `State "Delay"`, `Team1 West
      Indies/WI`, `Team2 Pakistan/PAK`, `InningsID 3`, `BatTeamShort "WI"`,
      `78/4`, `Overs 26.2`, striker `Tagenarine Chanderpaul 35(92)`, non-striker
      `Justin Greaves 17(24)`, `Valid true`. Assert the whole struct, not a
      field or two.
- [x] The same fixture proves unknown fields are ignored: `miniscore` carries 25
      keys and we read 5.
- [x] `leanback_null_miniscore.json` → the nil branch at
      [cricket.go:312](cricket.go#L312). `Valid` still true, score fields zero,
      no panic.
- [x] `leanback_no_current_innings.json` → falls back to `batTeamScore` for
      runs/wickets; `BatTeamShort` and `Overs` are left empty/zero. Assert
      `BatTeamName()` degrades to the empty short name rather than naming the
      wrong team.
- [x] `leanback_score_disagrees.json` → the innings line (`78/4`) wins over
      `batteamscore` (`999/9`). Without this, the primary and fallback paths are
      indistinguishable, because in real data they agree.
- [x] Empty `matchheaders` → no panic, `Title()` returns `" vs "`.

**`currentInnings`.**

- [x] Returns the line whose `inningsid` matches.
- [x] Returns nil when no line matches (`leanback_no_current_innings.json`).
- [x] `leanback_innings_current_not_first.json` — the active innings is **last**
      in the array. Required: real responses arrive newest-first, so the active
      innings happens to be element 0 and a naive "take the first line"
      implementation would pass against the real fixture by luck.

**`fetchLiveIndiaMatch`.**

- [x] `live_matches_india.json` → match `152496` returned, with the quota value.
- [x] `live_matches_no_india.json` → `(nil, remaining, nil)`. Nil match is *not*
      an error. (This is the real capture: 4 live matches, none involving India.)
- [x] `live_matches_null_wrapper.json` → the two malformed series (one with
      `"seriesAdWrapper": null`, one with the key absent) are skipped without
      panic ([cricket.go:142](cricket.go#L142)) and match `152496` is still
      found behind them.
- [x] `live_matches_india_complete.json` → the only India match is `Complete`,
      so `isWatchable` filters it and the result is nil.
- [x] `live_matches_india_a_women.json` → **nil**. India A and India Women are
      both live; neither counts.
- [x] The *first* watchable India match wins when several are live.
- [x] Malformed JSON body → decode error returned, no panic, quota still
      reported.

**`get` — headers and errors.**

- [x] `x-ratelimit-requests-remaining: 42` → `remaining == 42`.
- [x] Header absent → `remaining == -1`.
- [x] Header non-numeric → `remaining == -1` (not a crash, not 0 — 0 would trip
      the quota guard).
- [x] Request carries `X-RapidAPI-Key` and `X-RapidAPI-Host`.
- [x] Non-200 (429, 500) → error is a `*apiError` carrying status *and* raw
      body, and the quota value is still returned alongside it.
      `controller.logActivityError` type-asserts on this with `errors.As`
      ([main.go:287](main.go#L287)), so the concrete type is load-bearing.
- [x] `apiError.Error()` string is `"<path> returned status <code>"` — the
      comment at [cricket.go:85](cricket.go#L85) says this wording is
      deliberately preserved for log continuity.
- [x] A cancelled context aborts the request and returns an error.

**Team filters** — table-driven.

- [x] `isIndia`: `{TeamSName: "IND"}` true; `{TeamName: "India"}` true;
      `{TeamSName: "INDA", TeamName: "India A"}` **false**;
      `{TeamSName: "INDW", TeamName: "India Women"}` **false**; `AUS` false;
      zero value false.
- [x] `involvesIndia`: matches on team1, on team2, neither.
- [x] `isWatchable`: `""`, `"Complete"`, `"Abandoned"`, `"No result"` false —
      case-insensitively, and with surrounding whitespace. `"In Progress"`,
      `"Innings Break"`, `"Toss"`, `"Preview"`, `"Delay"`, `"Stumps"`, `"Rain"`
      true. (`"Delay"` and `"Complete"` are both attested in the real capture.)

The India cases are not busywork: CLAUDE.md states the exclusion of India A and
the women's side is **on purpose**. The test is what makes that survive a
well-meaning "fix".

**`fetchMatchScore`.**

- [x] Happy path returns a populated `ScoreState` with the right `MatchID`.
- [x] Non-200 → zero `ScoreState`, `Valid == false`, error returned. The
      `Valid` flag gates seeding, so a failed fetch must never look seeded.

### `controller_test.go`

Build a helper that returns a `*controller` wired to a fake server, a capturing
notifier, and a request counter.

**Seeding** — the invariant CLAUDE.md flags hardest.

- [x] First `watch` call after a `discover` stores `prev` and sends **zero**
      messages, even when the fixture contains a completed milestone and a
      fallen wicket.
- [x] Second `watch` call diffs against that seed and does notify.
- [x] `discover` resets `prev` to the zero value when it adopts a new match
      ([main.go:191](main.go#L191)), so a second match never diffs against the
      first one's tail.

**Mutual exclusion** — budget conservation, per CLAUDE.md.

- [x] `discover` with `matchID != 0` makes **zero** HTTP requests.
- [x] `watch` with `matchID == 0` makes **zero** HTTP requests.
- [x] Both make zero requests while `quotaPaused` is true.

**Match lifecycle.**

- [x] Match already terminal on the seeding poll → `logDone`, `clearMatch`,
      no notification, `matchID` back to 0 ([main.go:224](main.go#L224)).
- [x] Match goes terminal mid-watch → the result message fires **and**
      `matchID` returns to 0, so discovery takes over again.
- [x] A fetch error leaves `matchID` and `prev` untouched (no lost baseline)
      and does not notify.

**Quota guard.**

- [x] `remaining <= lowQuotaThreshold` (8) sets `quotaPaused` and sends the
      warning.
- [x] The warning is sent **exactly once** across many subsequent calls
      (`quotaAlerted`).
- [x] Once paused, further `discover` / `watch` calls make no requests.
- [x] `remaining == -1` (header missing) never trips the guard
      ([main.go:267](main.go#L267)) — a missing header must not be read as
      "quota exhausted".
- [x] `remaining == 9` does not trip it; `remaining == 8` does (boundary).

**Fired-message capture.**

- [x] `recordSend` → `takeFired` returns the messages and clears the buffer.
- [x] `resetFired` discards anything captured earlier, so a poll's activity
      line reports only that poll's events.
- [x] A `watch` poll that fires three events logs all three in one activity
      line.

**Small wiring helpers.**

- [x] `envDuration`: unset → default; `"45s"` → 45s; `"nonsense"` → default,
      with no panic and no exit.
- [x] `runEvery` calls `fn` immediately before the first tick.
- [x] `runEvery` returns promptly when the context is cancelled, and does not
      call `fn` again afterwards.
- [x] `logActivityError` routes a `*apiError` to `logAPIError` (status + body
      preserved) and anything else to `logError`.

**Concurrency.**

- [x] A test that runs `discover` and `watch` concurrently in a loop, under
      `-race`, with the fake server returning a mix of responses. It asserts
      nothing beyond "no race, no panic" — that is the point.

---

## Tier 2 — the bug that already bit

`activity_test.go`. Cheap, and directly encodes a CLAUDE.md rule.

**The disabled logger.** Per [LEARNINGS.md](LEARNINGS.md), treating a failed log
open as fatal crash-looped the service roughly 6900 times. The contract now is:
every `activity.*` call must be safe on a logger with a nil file.

- [x] Call **all** of `logDiscovery`, `logSeed`, `logWatch`, `logDone`,
      `logAPIError`, `logError`, `writeLine` and `close` on
      `newDisabledActivityLogger()`. Each is a silent no-op; none panics.
      Add every new `activity.*` method to this test as it is written.
- [x] `close` is idempotent — calling it twice does not panic.

**Rotation.**

- [x] With a small `maxBytes`, writing past the cap renames the file to
      `activity.log.1` and starts a fresh `activity.log`; both exist, and the
      new file contains only the newest line.
- [x] A second rotation overwrites the previous `.1` backup (only one backup is
      kept, by design).
- [x] `size` tracking is correct after rotation, so the next rotation happens at
      the right point rather than immediately.
- [x] Concurrent `writeLine` calls from several goroutines under `-race`
      produce well-formed, non-interleaved lines.

**Construction and formatting.**

- [x] `newActivityLogger` creates a missing parent directory.
- [x] `newActivityLogger` on an unwritable directory (chmod `0o500`, mirroring
      the systemd `ReadOnlyPaths` sandbox) returns an error and does **not**
      panic. Skip this case when the test runs as root.
- [x] `newActivityLogger` on an existing file appends rather than truncating,
      and picks up the existing size.
- [x] Every written line starts with a `2006-01-02 15:04:05` timestamp and ends
      with exactly one `\n`.
- [x] `oneLine` collapses `\r\n`, `\n`, `\r` and `\t` to spaces and trims —
      feed it a multi-line HTML error body, which is what a RapidAPI 429
      actually returns.
- [x] `quotaStr(-1)` → `"unknown"`; `quotaStr(0)` → `"0"`; `quotaStr(42)` →
      `"42"`.
- [x] `logWatch` with no events → `"no change"`; with three events → the
      `"3 event(s): … | … | …"` form.

---

## Tier 3 — closing the notifier's real gaps

Extend `notifier_test.go`. The existing helpers (`newTestNotifier`, `base`,
`wantOne`) stay; add a `wantAll(t, sent, substrs...)` for multi-message cases.

**Multi-event polls.** Every existing test asserts exactly one message, so
nothing pins what happens when a single poll contains several events — which is
the normal case at a 10-minute cadence.

- [x] Wicket + team-50 crossing + a batter fifty in one diff → all three
      messages, in the order `checkAndNotify` emits them.
- [x] Match start where `prev.InningsID == 0` → only the start message; the
      guard at [notifier.go:71](notifier.go#L71) suppresses the rest.
- [x] **The Step 0b(a) truth table**, as a single table test — all five rows.
      `TestMatchStartRainDelay` is the one that matters: `Preview`(inn 0) →
      `Delay`(inn 0) is silent, and `Delay`(inn 3) → `In Progress`(inn 3) is
      silent. Both are real states taken from the captured Day 3 fixture.
- [x] A table test over every attested state (`In Progress`, `Delay`,
      `Innings Break`, `Complete`, `Preview`, `Toss`, plus the newly added
      `Rain`, `Wet Outfield`, `Inspection`) asserting `isPreMatch` /
      `isTerminal` for each, so a new state value can't silently change meaning.
- [x] Mid-match break states (`Stumps`, `Lunch`, `Tea`, `Drinks`) are **not**
      pre-match — pin this, since adding them would resurrect the day-3 bug.

**Early returns.**

- [x] Match completes *and* runs jumped 50+ in the same poll → only the result
      message ([notifier.go:59](notifier.go#L59)).
- [x] Terminal state with an empty `Status` → falls back to `"match ended"`.
- [x] Innings change → only the end-of-innings message, even though runs and
      wickets both dropped ([notifier.go:64](notifier.go#L64)). A regression
      here would announce a phantom collapse.
- [x] Innings change where `curr.InningsID == 0` → nothing fires.
- [x] Already-terminal `prev` → no messages at all.

**Batter milestones.**

- [x] Strike rotation: striker and non-striker swap places between polls;
      `runsForBatter` finds each by name, and no milestone is announced twice.
- [x] 45 → 105 in one poll reports **100 only**, not 50 then 100
      (`highestMilestone`).
- [x] A batter already past 50 in `prev` stays silent.
- [x] **Step 0b(b)**: an unknown batter appearing on 60 (retired hurt returning,
      or resuming after missed polls) fires **nothing** — `runsForBatter`
      reports them as having no baseline.
- [x] The complementary case still works: a batter present in `prev` on 48 and
      now on 54 fires normally. Both branches of the new `known` flag covered.
- [x] `runsForBatter` directly: striker hit, non-striker hit, unknown name →
      `(0, false)`, and an empty name against an empty-named batter does not
      count as a match.
- [x] Empty batter name is skipped ([notifier.go:88](notifier.go#L88)).
- [x] 150 and 200 milestones fire (only 50 and 100 are exercised today).

**`formatWicket`** — regex-driven, currently 75% covered, and the most likely
place for silent formatting drift.

- [x] **The real string**, verbatim from the capture:
      `"Shai Hope   b Khurram Shahzad 10(8)  - 43/4 in 16.2 ov."` — note the
      three spaces before `b` and two after `10(8)`. Verified to yield
      `"💥 Wicket! Shai Hope out for 10. …"`. Irregular whitespace is normal in
      this field; keep this case even if the others are trimmed.
- [x] Standard caught: `"Kohli c Smith b Starc 34(40) - 187/3 in 30.1 ov."`
- [x] Bowled: `"Rohit b Cummins 12(19) …"`
- [x] LBW, run out, stumped, hit wicket, retired, `c& b` — one case each.
- [x] Name parses but runs do not → the `"X out. <score>"` form.
- [x] `LastWkt` empty or unparseable → the bare `"💥 Wicket! India 148/3"`
      fallback.
- [x] A multi-word name with initials, e.g. `"KL Rahul c … 8(11)"`.

**Remaining helpers.**

- [x] `ordinal`: 1, 2, 3, 4 (currently 40% covered — only one branch is hit).
- [x] `crossedMultiple`: no crossing; exact boundary (149 → 150); a jump over
      two multiples (148 → 251 → 250); a decrease returns false.
- [x] `formatOvers`: `30.1`, `0.0`, `49.6`, and a whole number renders as
      `"38.0"` not `"38"`.
- [x] `isPreMatch` / `isTerminal`: full table including `""`, mixed case, and
      padded whitespace.
- [x] `notify` when `send` returns an error → the error is swallowed and the
      *following* messages in the same poll still go out. Today a failing
      sender is never exercised.

---

## Fixtures

**Already captured.** `testdata/` holds twelve fixtures, built from two real
RapidAPI responses taken 2026-07-28 during the WI vs PAK 1st Test (match
`152496`, Day 3) — cost: 2 of the 200 monthly requests. See
[testdata/README.md](testdata/README.md) for the provenance of each file and how
the derived ones were produced.

All twelve have been verified to decode through the current structs and
`toScoreState`. Tests should read them with `os.ReadFile`; never generate
payloads inline.

The capture landed on a Test match, which is richer than the ODI-shaped data the
existing tests assume: three innings in `inningsscores`, and irregular
whitespace in `lastwkt` (`"Shai Hope   b Khurram Shahzad 10(8)  - …"`, three
spaces before the `b`) that a hand-written fixture would never have contained.

Do not regenerate these from Go structs. A fixture's value is that it breaks
visibly when the upstream shape changes — the failure mode this project should
actually fear, given the dead free Cricbuzz endpoints in
[LEARNINGS.md](LEARNINGS.md).

## Conventions

- Table-driven with `t.Run` subtests for the pure helpers; fixture-driven for
  anything crossing the API boundary.
- Keep the existing naming: `TestThingBehaviour`, `t.Helper()` on assertion
  helpers, `t.Fatalf` with the actual value in the message.
- No third-party assertion library. The project has exactly one dependency
  (`godotenv`) and should keep it that way.
- No `time.Sleep` in tests. Drive the loop functions directly, or use a
  millisecond ticker with a cancellable context.
- Fake servers via `httptest.NewServer`, always `defer srv.Close()`.
- Use `t.TempDir()` for activity-log tests so nothing touches the real
  `logs/` directory — which the systemd sandbox has a carve-out for, and which
  the running service may be writing to.

## Running

```sh
go test ./...                 # fast path
go test -race ./...           # required before committing
go vet ./...
gofmt -l .                    # must print nothing

go test -coverprofile=cov.out ./... && go tool cover -func=cov.out
go tool cover -html=cov.out   # line-by-line, in a browser
```

Nothing above touches the network, so the full suite is free to run as often as
you like.

## Targets

| File | Now | Target |
|------|-----|--------|
| `notifier.go` | ~85% | 95%+ |
| `cricket.go` | 0% | 85%+ (excluding `NewCricketClient`) |
| `main.go` | 0% | 75%+ (excluding `main` and `sendTelegram`) |
| `activity.go` | 0% | 85%+ |
| **total** | **16%** | **80%+** |

`main()` and `sendTelegram` stay untested: one is process wiring, the other is a
four-line HTTP POST that CLAUDE.md marks as deliberately untouched. Everything
else is reachable.
