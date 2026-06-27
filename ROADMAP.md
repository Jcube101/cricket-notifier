# Roadmap

## v1 — current

Shipped and running as a systemd service:

- Six notifications: match start, wicket, batting milestone (50/100/150/200),
  team milestone (every 50), innings change, match result.
- Data from the unofficial Cricbuzz API via RapidAPI (free Basic plan,
  200 requests/month).
- Two mutually-exclusive polling loops (discovery + watch) with a quota guard.
- In-memory state only. Telegram is the only output. Senior India men's side
  only.

## v2 — ideas, not committed

None of these are scheduled. They're recorded so the next person (or the next
six-months-later version of me) doesn't have to rediscover them.

- **Match format filter.** A `.env` toggle to opt in/out of Test, ODI and T20
  matches — e.g. skip the five-day Tests that quietly eat the API budget, or
  watch only white-ball cricket. The format is already available on every match
  (`matchFormat`), so this is mostly a filter in `fetchLiveIndiaMatch`.

- **Daily quota summary.** A once-a-day Telegram message reporting how many of
  the 200 monthly requests remain, so the budget doesn't sneak up on you. The
  remaining count is already known from the `x-ratelimit-requests-remaining`
  header on every call.

- **Persistent state across restarts (SQLite).** Save the last-seen snapshot so
  a restart mid-match doesn't lose its baseline. Would close the "events during
  downtime are missed" gap and remove the in-memory tradeoff described in
  SPEC.md. This is the single biggest robustness improvement available.

- **Alternative free data source.** If 200 requests/month proves too tight, or
  RapidAPI changes terms, evaluate another source. The free direct Cricbuzz
  endpoints were already dead when v1 was built (see LEARNINGS.md), so this
  means real research, not a quick swap.
