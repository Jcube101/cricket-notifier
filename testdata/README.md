# testdata

API response fixtures for the test suite. See [TESTING.md](../TESTING.md).

## Provenance

Two files are **real, unmodified** RapidAPI Cricbuzz responses, captured
2026-07-28 during the WI vs PAK 1st Test (match id `152496`, Day 3):

| File | Endpoint |
|------|----------|
| `live_matches_no_india.json` | `GET /matches/v1/live` |
| `leanback_test_day3.json` | `GET /mcenter/v1/152496/leanback` |

Only formatting changed (pretty-printed, 2-space indent). No fields added,
removed or renamed. Neither response contains credentials.

Every other file is **derived** from those two by minimal edits, so all of them
keep the real payload's structure, key casing and extra fields. The derivation
for each is noted below. Cost: 2 of the 200 monthly requests.

## Why a real Test match is a good baseline

The capture happened to be a Test on Day 3, which is richer than the ODI-shaped
data the original tests assumed:

- **Three completed/ongoing innings** in `inningsscores`, so `currentInnings`
  has something real to select from.
- **`inningsscores` arrives newest-first** (innings 3, 2, 1). The active innings
  being *first* is a coincidence of this fixture — see
  `leanback_innings_current_not_first.json`.
- **Messy `lastwkt`**: `"Shai Hope   b Khurram Shahzad 10(8)  - 43/4 in 16.2 ov."`
  — three spaces before the `b`, two after `10(8)`. A hand-written fixture would
  have used single spaces and never exercised the `\s+` in `wktNamePattern`.
- **`miniscore` carries 25 keys; we read 5.** Proves the structs tolerate a
  payload far wider than they model.
- **The two endpoints disagree on `state`** for the same match at the same
  moment: `/matches/v1/live` said `"In Progress"`, leanback said `"Delay"`.
  See the note in TESTING.md — this is a real behaviour, not a capture error.

## The files

### `/matches/v1/live`

| File | Derivation | Used for |
|------|-----------|----------|
| `live_matches_no_india.json` | **real** | no India match → `(nil, nil)`; also has 2 live + 2 `Complete` matches for `isWatchable` |
| `live_matches_india.json` | WI → `India`/`IND` | happy path: the match is found |
| `live_matches_india_a_women.json` | WI → `India A`/`INDA`, SOUW → `India Women`/`INDW` | the deliberate exclusion — must find **nothing** |
| `live_matches_india_complete.json` | India match forced to `state: "Complete"` | terminal India match is filtered out |
| `live_matches_null_wrapper.json` | prepends one series with `"seriesAdWrapper": null` and one with the key absent | nil-safety; the India match later in the list is still found |
| `live_matches_india_warmup_then_real.json` | **hand-built**, not derived from the two real captures — modeled on a live `/matches/v1/live` observation made 2026-08-07 (match `169497`, `matchDesc: "3-Day Warm-up Match"`, India vs Sri Lanka XI) | exhibition-match filter: a live warm-up match is skipped and a real Test later in the list is returned |

### `/mcenter/v1/{id}/leanback`

| File | Derivation | Used for |
|------|-----------|----------|
| `leanback_test_day3.json` | **real** | full happy-path mapping; WI 78/4 in 26.2, innings 3 |
| `leanback_innings_current_not_first.json` | innings array re-sorted ascending, so active innings 3 is **last** | catches a naive "take the first line" `currentInnings` |
| `leanback_score_disagrees.json` | `batteamscore` set to `999/9`, innings line untouched | proves the innings line is preferred over `batteamscore` |
| `leanback_no_current_innings.json` | innings 3 line removed | the fallback branch; `BatTeamShort` ends up empty |
| `leanback_null_miniscore.json` | `"miniscore": null`, state `Preview` | match not started; `Valid` still true, no panic |
| `leanback_complete.json` | state `Complete`, result status | terminal detection and the result message |
| `leanback_innings_break.json` | innings 4 prepended (PAK 12/0), state `Innings Break`, `lastwkt` cleared | innings-change diffing |

## Refreshing

Fixtures do not expire — they pin the shape we decode, and a break is a signal
worth seeing. If you do recapture, keep `leanback_test_day3.json` and
`live_matches_no_india.json` byte-faithful to the API and regenerate the derived
files from them by the edits above.

Capturing costs one request per endpoint against the 200/month budget, so do it
deliberately, and never from a test.
