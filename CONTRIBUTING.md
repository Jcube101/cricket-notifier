# Contributing

This is a personal project. It's **not accepting external contributions** —
no pull requests, issues, or feature requests. It's shared as something to read
and fork, not a collaboration.

## Forking it for your own team

The code is small and easy to repoint at a different team or tournament:

- **Different team.** Change the filter in `cricket.go` — `isIndia` /
  `involvesIndia` decide which match gets watched. Swap the `"IND"` / `"India"`
  checks for the team short name and full name you want. That's the only change
  needed to follow a different side.

- **A whole tournament instead of one team.** Replace the team check in
  `fetchLiveIndiaMatch` with a series/tournament check (the match data carries
  `seriesName`), and return the first live match in that series.

- **Different cadence or budget.** The polling intervals are env vars
  (`DISCOVERY_INTERVAL`, `WATCH_INTERVAL`) and the quota guard threshold is a
  constant in `main.go`. Tune them to your own API plan.

You'll need your own RapidAPI key, Telegram bot token and chat id — see
[README.md](README.md) for how to get each.

## Dev workflow

If you're hacking on a fork, [CLAUDE.md](CLAUDE.md) has the day-to-day workflow:
file layout, how to rebuild and restart the service, and how to run the tests.
