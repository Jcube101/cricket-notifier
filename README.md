# Cricket Notifier

A small Go service that watches live India men's cricket matches and sends
Telegram messages when something worth knowing about happens.

It is deliberately **not** a ball-by-ball feed. The goal is the opposite: stay
out of your way until a match becomes worth your attention. You get a ping when
the match starts, when a wicket falls, when a batter or the team reaches a
milestone, when the innings turns over, and when the match is decided — and
nothing in between. If your phone is quiet, nothing is happening.

It runs as a systemd service on a Raspberry Pi, keeps all state in memory, and
talks to the (unofficial) Cricbuzz API exposed through RapidAPI.

## The six notifications

Each event is detected by comparing the latest score snapshot against the
previous one. The exact message text the bot sends:

| Event | When it fires | Example message |
|-------|---------------|-----------------|
| **Match start** | State moves from pre-match (preview/toss) to playing | `🏏 India vs Australia — Match started` |
| **Wicket** | The batting team's wicket count goes up | `💥 Wicket! Virat Kohli out for 34. India 187/3` |
| **Batting milestone** | A batter at the crease crosses 50, 100, 150 or 200 | `🎉 Rohit Sharma reaches 50! (54 off 38)` |
| **Team milestone** | The batting total crosses a multiple of 50 | `📊 India reach 200 (201/3 in 38.2 overs)` |
| **Innings change** | The innings id changes | `🔄 End of 1st innings. India 287/6` |
| **Match result** | State moves to Complete / Abandoned / No Result | `🏆 Match result: India won by 45 runs` |

Two more operational messages exist: a `🏏 Cricket Notifier is online` ping on
startup, and a `⚠️` warning if the monthly API quota is nearly spent (see
below).

If the wicket line can't be parsed for the batter's name, the wicket message
degrades gracefully to `💥 Wicket! India 187/3`.

## How it works

Two loops share a single piece of state — the id of the match currently being
watched — and are **mutually exclusive**: only one of them ever calls the API
at a time.

- **Discovery loop** (default every 6h): runs only while idle. Asks the API for
  the list of live matches and picks the first one involving the senior India
  side. When it finds one, it hands the match id to the watch loop and goes
  quiet.
- **Watch loop** (default every 10m): runs only while a match is active.
  Fetches the live mini-score, diffs it against the last snapshot, fires any
  notifications, and stores the new snapshot. When the match reaches a terminal
  state it clears the match and the discovery loop takes back over.

The first snapshot of any match is stored **without** notifying ("seeding"), so
the service never replays events that happened before it started watching.

Shutdown is clean: on `SIGTERM` (systemd stop) or `SIGINT` (Ctrl-C) the context
is cancelled, both loops exit, and any in-flight HTTP request is aborted.

## The quota constraint

The RapidAPI Cricbuzz **free "Basic" plan allows 200 requests per month.** That
is the binding limit — the 1000 requests/hour rate cap never comes into play.

This is why the intervals are measured in hours and minutes, not seconds, and
why the two loops never poll at the same time. Every response carries an
`x-ratelimit-requests-remaining` header. The service reads it on every call,
and once **8 or fewer** requests remain it:

1. stops making any further API calls, and
2. sends a single Telegram warning.

It stays paused for the life of the process; restart it to pick the quota back
up (a restart re-reads the live remaining count). At the defaults, a quiet day
costs ~4 requests and a single ODI costs ~48, so the budget comfortably covers
a few matches a month.

## Setup

### Prerequisites

- Go 1.24+
- A Raspberry Pi (or any linux/arm64 / linux box) with systemd
- A Telegram account and a RapidAPI account

### 1. RapidAPI key

Subscribe to the **Cricbuzz Cricket** API (publisher `cricketapilive`) on
RapidAPI and pick the free **Basic** plan. Copy the `X-RapidAPI-Key` value.

### 2. Telegram bot token and chat id

- Message [@BotFather](https://t.me/BotFather), create a bot, copy its token.
- Start a chat with your new bot, then find your chat id (e.g. message
  [@userinfobot](https://t.me/userinfobot), or read it from
  `https://api.telegram.org/bot<TOKEN>/getUpdates`).

### 3. Configure `.env`

Create `.env` in the project directory (it is gitignored):

```sh
BOT_TOKEN=123456:your-telegram-bot-token
CHAT_ID=your-chat-id
RAPIDAPI_KEY=your-rapidapi-key

# Optional — Go duration strings. Defaults shown.
# DISCOVERY_INTERVAL=6h
# WATCH_INTERVAL=10m
```

All three of `BOT_TOKEN`, `CHAT_ID` and `RAPIDAPI_KEY` are required; the service
exits immediately if any is missing.

### 4. Build

```sh
go build -o cricket-notifier .
```

### 5. Install as a systemd service

The unit file ([cricket-notifier.service](cricket-notifier.service)) runs the
binary as user `jcube`, loads `.env` via `EnvironmentFile`, and restarts on
failure with a 30s backoff.

```sh
sudo cp cricket-notifier.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now cricket-notifier
systemctl status cricket-notifier
journalctl -u cricket-notifier -f
```

After rebuilding the binary, restart the service:

```sh
go build -o cricket-notifier . && sudo systemctl restart cricket-notifier
```

## Tests

```sh
go test ./...
```

The tests cover the notification logic — one test per event — using a fake
sender, so they make no network calls and cost no API quota.

## Scope

India men's senior side only. "India A" and the women's side are excluded by
design. In-memory state, no database, no web UI — Telegram is the only output.
See [SPEC.md](SPEC.md) for the product decisions and [ROADMAP.md](ROADMAP.md)
for what might come next.
