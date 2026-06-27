# Spec

What this project is, the decisions behind it, and how each notification is
actually computed. This is the "why it is the way it is" document.

## Tech Stack

| Category | Technologies |
|---|---|
| Language | Go |
| APIs | Telegram Bot API, RapidAPI Cricbuzz |
| Infrastructure | Raspberry Pi, systemd |
| Testing | Go testing package |
| Config | godotenv |

## Product decisions

- **Free data source.** No paid API. The service uses the unofficial Cricbuzz
  API through RapidAPI's free Basic plan. This is the decision that constrains
  everything else (see the quota section).
- **India men's senior side only.** Matches are selected by team identity
  (`teamSName == "IND"` or `teamName == "India"`). "India A" and the women's
  side are intentionally out of scope.
- **Auto-detect live matches.** No configuration of which match to watch. The
  service discovers the current live India match on its own and starts watching
  it; when it ends, it goes back to looking.
- **No web UI, Telegram only.** The single output channel is a Telegram bot
  message. There is no dashboard, no HTTP server, nothing to log into.
- **In-memory state.** No database. The only state is the match currently being
  watched and its last score snapshot, held in memory.

## The six notifications and how they're detected

Every poll produces a `ScoreState` snapshot. `checkAndNotify(prev, curr)`
compares the previous snapshot to the current one. The checks run in this order,
and some of them stop further checking for that poll:

1. **Match start** — `🏏 <Team1> vs <Team2> — Match started`.
   Fires when `prev.State` is a pre-match state (empty, preview, upcoming,
   scheduled, toss) and `curr.State` is neither pre-match nor terminal.

2. **Match result** — `🏆 Match result: <status>`.
   Fires when `prev.State` is not terminal and `curr.State` is terminal
   (Complete, Abandoned, No Result). The API's own status line is used as the
   result text. This check returns early — once the match is over, nothing else
   is worth diffing.

3. **Innings change** — `🔄 End of <ordinal> innings. <battingTeam> <runs>/<wkts>`.
   Fires when both snapshots have a non-zero innings id and the ids differ. It
   reports the **previous** innings' final score (the one that just ended), then
   returns early — the score has reset for the new innings, so the wicket and
   milestone checks below would compare across an innings boundary and produce
   garbage.

The remaining three checks only run when both snapshots are in the **same,
continuing innings** (same non-zero innings id):

4. **Wicket** — `💥 Wicket! <name> out for <runs>. <battingTeam> <runs>/<wkts>`.
   Fires when `curr.Wickets > prev.Wickets`. The dismissed batter's name and
   score are parsed from the API's pre-formatted "last wicket" string with a
   regex; if parsing fails the message degrades to just the team score.

5. **Team milestone** — `📊 <battingTeam> reach <n> (<runs>/<wkts> in <overs> overs)`.
   Fires when the team total crosses a multiple of 50. `crossedMultiple`
   reports the highest multiple of 50 that `curr` has reached but `prev` had
   not, so a jump from 148 to 201 across one poll reports 200 (not 150 and 200).

6. **Batting milestone** — `🎉 <name> reaches <n>! (<runs> off <balls>)`.
   For each batter currently at the crease, fires when they cross 50, 100, 150
   or 200. The batter's previous runs are looked up **by name** (strike rotates,
   so a player moves between striker and non-striker between polls); an unknown
   batter is treated as having 0 previous runs. Only the highest milestone
   crossed since the last poll is announced.

## The quota constraint and why it shaped the design

The free plan allows **200 API requests per month**. With seconds-scale polling,
a single match would blow the entire month's budget. So:

- The two loops are **mutually exclusive** — discovery only polls while idle,
  watch only polls while a match is live — so the service never spends two
  requests where one would do.
- Intervals are coarse by default (discovery 6h, watch 10m) and tunable via
  `.env`, never hard-wired to short values.
- A **quota guard** reads `x-ratelimit-requests-remaining` from every response.
  At 8 or fewer remaining it stops all API calls and sends one warning, so the
  service degrades quietly instead of hammering an exhausted quota.

The coarse polling is also a feature, not just a budget compromise: the product
goal is "tell me when a match is worth watching," for which 10-minute
granularity is fine. Several events between polls collapse sensibly — the team
milestone reports the highest 50 crossed, a wicket is announced with the current
score, and batter milestones report the highest level reached.

## In-memory state: the known tradeoff

State is not persisted. On restart, the watch loop **re-seeds** from the live
score — it stores the first post-restart snapshot without notifying — so it does
not replay events that already happened.

The consequence is twofold:

- **Events during downtime are missed.** If the service is down while a wicket
  falls or a fifty is reached, that moment is gone; the new baseline is whatever
  the score is when it comes back up. This is the dominant tradeoff.
- **A duplicate notification is possible on restart.** The startup `🏏 ... is
  online` ping is re-sent every start, and if the quota was already low the `⚠️`
  warning fires again (its "already alerted" flag is in memory and resets). The
  six match events are protected from re-firing by seeding, but these
  process-lifetime messages are not.

Both are acceptable for a personal notifier. Persisting the last snapshot (e.g.
SQLite) would close the downtime gap; it's noted as a v2 idea in
[ROADMAP.md](ROADMAP.md).
