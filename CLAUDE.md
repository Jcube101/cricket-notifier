# CLAUDE.md

Working notes for developing this project. For the product description and
setup, see [README.md](README.md).

## Project layout

Single Go package (`package main`), four source files:

| File | Responsibility |
|------|----------------|
| `cricket.go` | The RapidAPI Cricbuzz client. HTTP plumbing, the response structs for `/matches/v1/live` and `/mcenter/{id}/leanback`, and the two fetchers: `fetchLiveIndiaMatch` and `fetchMatchScore`. Also defines `ScoreState` — the flat snapshot the rest of the code diffs — and maps the messy API JSON onto it. |
| `notifier.go` | The "what changed?" logic. `checkAndNotify(prev, curr ScoreState)` diffs two snapshots and sends one Telegram message per detected event. All message formatting and the small pure helpers (`crossedMultiple`, `highestMilestone`, `formatWicket`, etc.) live here. |
| `main.go` | Wiring. Loads `.env`, builds the client and notifier, runs the two polling loops (`discover` / `watch`) as goroutines, owns the shared `controller` state and the quota guard, and handles graceful shutdown. Contains the untouched `sendTelegram` primitive. |
| `notifier_test.go` | One test per notification event, driven through a fake sender. No network, no API cost. |

## Common tasks

Rebuild and restart the service:

```sh
go build -o cricket-notifier . && sudo systemctl restart cricket-notifier
```

Run the tests:

```sh
go test ./...
```

Inspect the running service:

```sh
systemctl status cricket-notifier
journalctl -u cricket-notifier -f
```

A sudoers drop-in is already configured, so `systemctl` commands for this
service run **without a password** — Claude Code can restart, stop, start and
check status directly.

## Loop intervals

Two optional env vars (Go duration strings) tune the polling cadence; both have
defaults baked in as constants in `main.go`:

- `DISCOVERY_INTERVAL` — how often the idle discovery loop checks for a live
  India match. Default `6h`.
- `WATCH_INTERVAL` — how often an active match is polled. Default `10m`.

Keep these generous. The API budget is 200 requests/month; seconds-scale
polling would exhaust it in a single match.

## Things to keep in mind when changing the code

- **India filter.** `isIndia` in `cricket.go` matches only the senior men's
  side (`teamSName == "IND"` or `teamName == "India"`). "India A" (INDA) and the
  women's side are excluded on purpose. Broaden `involvesIndia` if that ever
  needs to change.

- **Quota guard.** `controller.noteQuota` in `main.go` reads the
  `x-ratelimit-requests-remaining` header from every API response. When it drops
  to `lowQuotaThreshold` (8) or below, it sets `quotaPaused`, which makes both
  loops skip their API calls, and sends one Telegram warning. It does not
  auto-resume — restart the service to re-read the live quota.

- **Mutual exclusion.** `discover` only acts when `matchID == 0`; `watch` only
  acts when `matchID != 0`. This is intentional budget conservation, not a
  technical limitation. Don't make them poll concurrently.

- **Seeding.** The watch loop stores the first snapshot of a match without
  calling `checkAndNotify`, so past events aren't replayed. Preserve this when
  touching `watch`.

- **State lives only in memory** (`controller.prev`). A restart loses it and
  re-seeds from the live score. See SPEC.md for the tradeoff.
