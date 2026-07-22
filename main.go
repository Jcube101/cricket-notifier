package main

// main.go — wiring and the two polling loops.
//
//   Discovery loop: while idle, every DISCOVERY_INTERVAL it asks the API for a
//                   live India match. When it finds one, it hands the match id
//                   to the watch loop and goes quiet.
//   Watch loop:     while a match is active, every WATCH_INTERVAL it fetches the
//                   score, diffs it against the previous snapshot and fires
//                   notifications. When the match ends it clears the match and
//                   the discovery loop takes over again.
//
// The two loops are mutually exclusive on purpose: we never discover while
// watching, because the RapidAPI free plan allows only ~200 requests/month and
// every call counts. A quota guard stops all API calls (with one heads-up
// message) before we blow through the monthly allowance.
//
// sendTelegram is the original notification primitive and is left untouched.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"
)

const (
	// Defaults are deliberately gentle to respect the 200 requests/month quota.
	// Override with DISCOVERY_INTERVAL / WATCH_INTERVAL in .env (Go durations,
	// e.g. "3h", "10m", "45s").
	//
	// Rough monthly cost: discovery ≈ (24h / interval) calls per idle day;
	// watch ≈ (match hours × 60 / minutes) calls per match. At 6h/10m a quiet
	// day costs 4 calls and one ODI costs ~48.
	defaultDiscoveryInterval = 6 * time.Hour
	defaultWatchInterval     = 10 * time.Minute

	// When this few monthly requests remain, stop calling the API to avoid
	// hammering a quota that's already spent.
	lowQuotaThreshold = 8

	// activityLogPath is the persistent, rotation-safe activity log. It is
	// relative to the working directory (the project dir, same assumption
	// godotenv.Load makes for .env).
	activityLogPath = "logs/activity.log"
)

func main() {
	if err := godotenv.Load(); err != nil {
		slog.Error("could not load .env", "err", err)
		os.Exit(1)
	}

	token := os.Getenv("BOT_TOKEN")
	chatID := os.Getenv("CHAT_ID")
	apiKey := os.Getenv("RAPIDAPI_KEY")
	if token == "" || chatID == "" || apiKey == "" {
		slog.Error("BOT_TOKEN, CHAT_ID and RAPIDAPI_KEY must all be set in .env")
		os.Exit(1)
	}

	discoveryInterval := envDuration("DISCOVERY_INTERVAL", defaultDiscoveryInterval)
	watchInterval := envDuration("WATCH_INTERVAL", defaultWatchInterval)

	activity, err := newActivityLogger(activityLogPath, activityLogMaxBytes)
	if err != nil {
		slog.Error("could not open activity log", "path", activityLogPath, "err", err)
		os.Exit(1)
	}
	defer activity.close()

	notifier := NewNotifier(token, chatID)
	ctrl := &controller{
		client:   NewCricketClient(apiKey),
		notifier: notifier,
		activity: activity,
	}

	// Wrap the notifier's send so every message it emits is also captured for
	// the activity log. This is purely additive — the original send still runs
	// and the notification/scoring logic is untouched. The watch loop reads the
	// captured messages back to summarise which events a poll fired.
	baseSend := notifier.send
	notifier.send = func(text string) error {
		ctrl.recordSend(text)
		return baseSend(text)
	}

	// Cancel the context on Ctrl-C or SIGTERM (systemd stop) for a clean exit.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := sendTelegram(token, chatID, "🏏 Cricket Notifier is online"); err != nil {
		slog.Error("failed to send startup message", "err", err)
	}
	slog.Info("started",
		"discoveryInterval", discoveryInterval.String(),
		"watchInterval", watchInterval.String())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); runEvery(ctx, discoveryInterval, ctrl.discover) }()
	go func() { defer wg.Done(); runEvery(ctx, watchInterval, ctrl.watch) }()

	<-ctx.Done()
	slog.Info("shutting down…")
	wg.Wait()
	slog.Info("stopped")
}

// controller is the shared state between the two loops. The mutex protects the
// active match id and its last-seen snapshot.
type controller struct {
	client   *CricketClient
	notifier *Notifier
	activity *activityLogger

	mu           sync.Mutex
	matchID      int        // 0 when no match is being watched
	prev         ScoreState // last snapshot of the active match (Valid=false until seeded)
	quotaPaused  bool       // true once the monthly quota is (nearly) spent
	quotaAlerted bool       // ensures the low-quota warning is sent only once

	firedMu sync.Mutex // guards fired
	fired   []string   // messages sent since the last resetFired (for activity logging)
}

// recordSend captures a message the notifier emitted. Called from the wrapped
// send func, so it runs on whichever goroutine triggered the notification.
func (c *controller) recordSend(text string) {
	c.firedMu.Lock()
	c.fired = append(c.fired, text)
	c.firedMu.Unlock()
}

// resetFired clears the capture buffer so the next takeFired reports only
// messages emitted after this point.
func (c *controller) resetFired() {
	c.firedMu.Lock()
	c.fired = nil
	c.firedMu.Unlock()
}

// takeFired returns and clears the captured messages.
func (c *controller) takeFired() []string {
	c.firedMu.Lock()
	defer c.firedMu.Unlock()
	f := c.fired
	c.fired = nil
	return f
}

// discover runs on the discovery ticker. It only does anything while idle.
func (c *controller) discover(ctx context.Context) {
	c.mu.Lock()
	idle := c.matchID == 0
	paused := c.quotaPaused
	c.mu.Unlock()

	if paused || !idle {
		return
	}

	match, remaining, err := c.client.fetchLiveIndiaMatch(ctx)
	c.noteQuota(remaining)
	if err != nil {
		slog.Error("discovery failed", "err", err)
		c.logActivityError("discovery", err)
		return
	}
	if match == nil {
		slog.Info("discovery: no live India match", "quotaRemaining", remaining)
		c.activity.logDiscovery(false, 0, remaining)
		return
	}

	c.mu.Lock()
	c.matchID = match.MatchID
	c.prev = ScoreState{} // force the watch loop to seed before notifying
	c.mu.Unlock()

	c.activity.logDiscovery(true, match.MatchID, remaining)
	slog.Info("now watching match",
		"matchId", match.MatchID,
		"desc", fmt.Sprintf("%s vs %s %s", match.Team1.TeamName, match.Team2.TeamName, match.MatchDesc),
		"state", match.State)
}

// watch runs on the watch ticker. It only does anything while a match is active.
func (c *controller) watch(ctx context.Context) {
	c.mu.Lock()
	id := c.matchID
	prev := c.prev
	paused := c.quotaPaused
	c.mu.Unlock()

	if paused || id == 0 {
		return
	}

	curr, remaining, err := c.client.fetchMatchScore(ctx, id)
	c.noteQuota(remaining)
	if err != nil {
		slog.Error("watch fetch failed", "matchId", id, "err", err)
		c.logActivityError("watch", err)
		return
	}

	// Seeding: the first snapshot is stored without notifying, so we never
	// replay events that happened before we started watching.
	if !prev.Valid {
		if isTerminal(curr.State) {
			slog.Info("match already finished on first look; not watching", "matchId", id)
			c.activity.logDone(id, "already finished on first look; not watching")
			c.clearMatch()
			return
		}
		c.mu.Lock()
		c.prev = curr
		c.mu.Unlock()
		c.activity.logSeed(id, remaining, curr.State, curr.Runs, curr.Wickets)
		slog.Info("seeded match state", "matchId", id, "state", curr.State,
			"score", fmt.Sprintf("%d/%d", curr.Runs, curr.Wickets))
		return
	}

	// resetFired/takeFired bracket the diff so we log exactly the messages this
	// poll's checkAndNotify emitted (or "no change").
	c.resetFired()
	c.notifier.checkAndNotify(prev, curr)
	c.activity.logWatch(id, remaining, c.takeFired())

	if isTerminal(curr.State) {
		slog.Info("match finished", "matchId", id, "status", curr.Status)
		c.clearMatch()
		return
	}

	c.mu.Lock()
	c.prev = curr
	c.mu.Unlock()
}

// clearMatch returns the controller to the idle state so discovery resumes.
func (c *controller) clearMatch() {
	c.mu.Lock()
	c.matchID = 0
	c.prev = ScoreState{}
	c.mu.Unlock()
}

// noteQuota watches the remaining monthly request count and, once it runs low,
// pauses all API calls and sends a single heads-up message.
func (c *controller) noteQuota(remaining int) {
	if remaining < 0 || remaining > lowQuotaThreshold {
		return
	}
	c.mu.Lock()
	c.quotaPaused = true
	alert := !c.quotaAlerted
	c.quotaAlerted = true
	c.mu.Unlock()

	slog.Warn("RapidAPI monthly quota nearly spent; pausing API calls", "remaining", remaining)
	if alert {
		_ = c.notifier.send(fmt.Sprintf(
			"⚠️ Cricket Notifier paused: only %d API requests left this month. It will resume after the quota resets or a restart.",
			remaining))
	}
}

// logActivityError writes an error to the activity log, surfacing the raw HTTP
// status and response body when the failure came from a non-200 API response.
func (c *controller) logActivityError(where string, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		c.activity.logAPIError(where, ae.status, ae.body)
		return
	}
	c.activity.logError(where, err)
}

// runEvery calls fn immediately, then on every tick until the context is done.
func runEvery(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	fn(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn(ctx)
		}
	}
}

// envDuration reads a Go duration from the environment, falling back to def.
func envDuration(key string, def time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		slog.Warn("invalid duration in env; using default", "key", key, "value", raw, "default", def.String())
		return def
	}
	return d
}

func sendTelegram(token, chatID, text string) error {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)

	resp, err := http.PostForm(apiURL, url.Values{
		"chat_id": {chatID},
		"text":    {text},
	})
	if err != nil {
		return fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram returned status %d", resp.StatusCode)
	}

	return nil
}
