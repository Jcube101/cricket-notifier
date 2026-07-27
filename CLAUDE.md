# CLAUDE.md

Working notes for developing this project. For the product description and
setup, see [README.md](README.md).

## Project layout

Single Go package (`package main`), four source files:

| File | Responsibility |
|------|----------------|
| `cricket.go` | The RapidAPI Cricbuzz client. HTTP plumbing, the response structs for `/matches/v1/live` and `/mcenter/{id}/leanback`, and the two fetchers: `fetchLiveIndiaMatch` and `fetchMatchScore`. Also defines `ScoreState` — the flat snapshot the rest of the code diffs — and maps the messy API JSON onto it. |
| `notifier.go` | The "what changed?" logic. `checkAndNotify(prev, curr ScoreState)` diffs two snapshots and sends one Telegram message per detected event. All message formatting and the small pure helpers (`crossedMultiple`, `highestMilestone`, `formatWicket`, etc.) live here. |
| `main.go` | Wiring. Loads `.env`, builds the client, notifier and activity logger, runs the two polling loops (`discover` / `watch`) as goroutines, owns the shared `controller` state and the quota guard, and handles graceful shutdown. Contains the untouched `sendTelegram` primitive. |
| `activity.go` | The persistent activity log (`logs/activity.log`). A dependency-free, size-rotating text logger (5 MiB cap, one `activity.log.1` backup) that records one line per discovery check, watch poll (quota + diff result) and API error. Observability only — sits alongside `slog`, never affects notifications. |
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
tail -f logs/activity.log   # persistent app-level log, survives journal rotation
```

The activity log matters here because the host's journal lives on a small
`log2ram` RAM disk and only retains ~a day, so `journalctl` history is usually
gone by the time you debug. `logs/activity.log` is the durable record.

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

- **Activity log is observability, never a dependency.** `newActivityLogger`
  writes `logs/activity.log`, but if it can't open the file `main.go` warns and
  falls back to a disabled no-op logger — losing the log must never stop the
  service from starting. Keep it that way; every `activity.*` call must be safe
  on the disabled (nil-file) logger. This is not academic: a fatal
  `os.Exit(1)` here once crash-looped the service ~6900 times (see LEARNINGS.md).

- **The systemd sandbox makes the project dir read-only.** The unit sets
  `ProtectSystem=strict`, `ProtectHome=read-only` and
  `ReadOnlyPaths=<project>`, so the process cannot write anywhere by default.
  The activity log works only because `ReadWritePaths=<project>/logs` punches a
  writable hole — and systemd requires that `logs/` **already exist on disk**
  (it bind-mounts it). Any new on-disk write needs the same treatment.
