package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestController builds a *controller wired to a fake server (via the
// baseURL seam), a capturing notifier, and a request counter. It mirrors the
// send-wrapping main.go does, so fired messages land in both the returned
// slice (for assertions) and ctrl.fired (for activity-log tests).
func newTestController(t *testing.T, handler http.HandlerFunc) (*controller, *[]string, *int32) {
	t.Helper()
	var reqCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqCount, 1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	var sent []string
	ctrl := &controller{
		client:   newTestClient(srv),
		activity: newDisabledActivityLogger(),
	}
	ctrl.notifier = &Notifier{send: func(text string) error {
		sent = append(sent, text)
		ctrl.recordSend(text)
		return nil
	}}
	return ctrl, &sent, &reqCount
}

// --- Seeding -----------------------------------------------------------------

func TestWatchSeedsWithoutNotifying(t *testing.T) {
	// leanback_test_day3.json already contains a fallen wicket and a completed
	// milestone-worthy score; seeding must swallow both silently.
	body := readFixture(t, "leanback_test_day3.json")
	ctrl, sent, reqs := newTestController(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	})
	ctrl.matchID = 152496
	ctrl.prev = ScoreState{}

	ctrl.watch(context.Background())

	if len(*sent) != 0 {
		t.Fatalf("expected zero messages on the seeding poll, got %v", *sent)
	}
	if !ctrl.prev.Valid || ctrl.prev.Runs != 78 {
		t.Fatalf("expected prev seeded from the fetched score, got %+v", ctrl.prev)
	}
	if got := atomic.LoadInt32(reqs); got != 1 {
		t.Fatalf("expected exactly 1 request, got %d", got)
	}
}

func TestWatchSecondPollNotifies(t *testing.T) {
	seedBody := readFixture(t, "leanback_test_day3.json")
	const secondBody = `{
		"miniscore": {
			"batsmanstriker": {"name": "New Batter", "runs": 5, "balls": 10},
			"batsmannonstriker": {"name": "Justin Greaves", "runs": 17, "balls": 24},
			"lastwkt": "Tagenarine Chanderpaul c Rizwan b Abbas 35(92) - 80/5 in 27.0 ov.",
			"inningsscores": {"inningsscore": [{"inningsid": 3, "batteamshortname": "WI", "runs": 80, "wickets": 5, "overs": 27.0}]},
			"inningsid": 3,
			"batteamscore": {"teamid": 10, "teamscore": 80, "teamwkts": 5}
		},
		"matchheaders": {
			"state": "Delay",
			"status": "Day 3",
			"matchformat": "TEST",
			"team1": {"teamname": "West Indies", "teamsname": "WI"},
			"team2": {"teamname": "Pakistan", "teamsname": "PAK"}
		}
	}`

	var poll int32
	ctrl, sent, _ := newTestController(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&poll, 1) == 1 {
			w.Write(seedBody)
			return
		}
		w.Write([]byte(secondBody))
	})
	ctrl.matchID = 152496
	ctrl.prev = ScoreState{}

	ctrl.watch(context.Background()) // seeds, no message
	ctrl.watch(context.Background()) // diffs against the seed, should notify

	if len(*sent) == 0 {
		t.Fatalf("expected the second poll to notify, got none")
	}
	found := false
	for _, m := range *sent {
		if strings.Contains(m, "Wicket") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a wicket message among %v", *sent)
	}
}

func TestDiscoverResetsPrevOnNewMatch(t *testing.T) {
	body := readFixture(t, "live_matches_india.json")
	ctrl, _, reqs := newTestController(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	})
	ctrl.matchID = 0
	ctrl.prev = ScoreState{Valid: true, MatchID: 999, Runs: 500}

	ctrl.discover(context.Background())

	if ctrl.matchID != 152496 {
		t.Fatalf("expected matchID 152496, got %d", ctrl.matchID)
	}
	if ctrl.prev != (ScoreState{}) {
		t.Fatalf("expected prev reset to the zero value so it never diffs against the old match, got %+v", ctrl.prev)
	}
	if got := atomic.LoadInt32(reqs); got != 1 {
		t.Fatalf("expected exactly 1 request, got %d", got)
	}
}

// --- Mutual exclusion ----------------------------------------------------------

func TestDiscoverSkipsWhenActive(t *testing.T) {
	ctrl, _, reqs := newTestController(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{}"))
	})
	ctrl.matchID = 42

	ctrl.discover(context.Background())

	if got := atomic.LoadInt32(reqs); got != 0 {
		t.Fatalf("expected zero requests while a match is already active, got %d", got)
	}
}

func TestWatchSkipsWhenIdle(t *testing.T) {
	ctrl, _, reqs := newTestController(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{}"))
	})
	ctrl.matchID = 0

	ctrl.watch(context.Background())

	if got := atomic.LoadInt32(reqs); got != 0 {
		t.Fatalf("expected zero requests while idle, got %d", got)
	}
}

func TestBothSkipWhenQuotaPaused(t *testing.T) {
	ctrl, _, reqs := newTestController(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{}"))
	})
	ctrl.quotaPaused = true

	ctrl.matchID = 0
	ctrl.discover(context.Background())
	ctrl.matchID = 42
	ctrl.watch(context.Background())

	if got := atomic.LoadInt32(reqs); got != 0 {
		t.Fatalf("expected zero requests while quota-paused, got %d", got)
	}
}

// --- Match lifecycle ------------------------------------------------------------

func TestWatchTerminalOnSeedingPoll(t *testing.T) {
	body := readFixture(t, "leanback_complete.json")
	ctrl, sent, _ := newTestController(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	})
	ctrl.matchID = 152496
	ctrl.prev = ScoreState{}

	ctrl.watch(context.Background())

	if len(*sent) != 0 {
		t.Fatalf("expected no notification for a match already finished on first look, got %v", *sent)
	}
	if ctrl.matchID != 0 {
		t.Fatalf("expected matchID reset to 0, got %d", ctrl.matchID)
	}
}

func TestWatchTerminalMidMatch(t *testing.T) {
	body := readFixture(t, "leanback_complete.json")
	ctrl, sent, _ := newTestController(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	})
	ctrl.matchID = 152496
	ctrl.prev = ScoreState{
		Valid: true, State: "In Progress",
		Team1Name: "West Indies", Team2Name: "Pakistan", InningsID: 3,
	}

	ctrl.watch(context.Background())

	if len(*sent) != 1 || !strings.Contains((*sent)[0], "Match result") {
		t.Fatalf("expected exactly one match-result message, got %v", *sent)
	}
	if ctrl.matchID != 0 {
		t.Fatalf("expected matchID reset to 0 so discovery takes over again, got %d", ctrl.matchID)
	}
}

func TestWatchFetchErrorLeavesStateUntouched(t *testing.T) {
	ctrl, sent, _ := newTestController(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	prev := ScoreState{Valid: true, State: "In Progress", Runs: 100}
	ctrl.matchID = 152496
	ctrl.prev = prev

	ctrl.watch(context.Background())

	if ctrl.matchID != 152496 {
		t.Fatalf("expected matchID untouched after a fetch error, got %d", ctrl.matchID)
	}
	if ctrl.prev != prev {
		t.Fatalf("expected prev untouched after a fetch error, got %+v", ctrl.prev)
	}
	if len(*sent) != 0 {
		t.Fatalf("expected no notification on a fetch error, got %v", *sent)
	}
}

// --- Quota guard -----------------------------------------------------------------

func TestNoteQuotaTripsAtThreshold(t *testing.T) {
	ctrl, sent, _ := newTestController(t, func(w http.ResponseWriter, r *http.Request) {})
	ctrl.noteQuota(5)
	if !ctrl.quotaPaused {
		t.Fatalf("expected quotaPaused true")
	}
	if len(*sent) != 1 {
		t.Fatalf("expected exactly one warning message, got %v", *sent)
	}
}

func TestNoteQuotaWarnsOnlyOnce(t *testing.T) {
	ctrl, sent, _ := newTestController(t, func(w http.ResponseWriter, r *http.Request) {})
	for i := 0; i < 5; i++ {
		ctrl.noteQuota(3)
	}
	if len(*sent) != 1 {
		t.Fatalf("expected exactly one warning across repeated calls, got %d: %v", len(*sent), *sent)
	}
}

func TestNoteQuotaPausesSubsequentCalls(t *testing.T) {
	ctrl, _, reqs := newTestController(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"typeMatches":[]}`))
	})
	ctrl.noteQuota(3)
	ctrl.matchID = 0
	ctrl.discover(context.Background())
	if got := atomic.LoadInt32(reqs); got != 0 {
		t.Fatalf("expected no requests once quota-paused, got %d", got)
	}
}

func TestNoteQuotaMissingHeaderNeverTrips(t *testing.T) {
	ctrl, _, _ := newTestController(t, func(w http.ResponseWriter, r *http.Request) {})
	ctrl.noteQuota(-1)
	if ctrl.quotaPaused {
		t.Fatalf("expected a missing header (-1) to never trip the quota guard")
	}
}

func TestNoteQuotaBoundary(t *testing.T) {
	t.Run("9 does not trip", func(t *testing.T) {
		ctrl, _, _ := newTestController(t, func(w http.ResponseWriter, r *http.Request) {})
		ctrl.noteQuota(9)
		if ctrl.quotaPaused {
			t.Fatalf("expected remaining=9 to not trip the guard")
		}
	})
	t.Run("8 trips", func(t *testing.T) {
		ctrl, _, _ := newTestController(t, func(w http.ResponseWriter, r *http.Request) {})
		ctrl.noteQuota(8)
		if !ctrl.quotaPaused {
			t.Fatalf("expected remaining=8 to trip the guard")
		}
	})
}

// --- Fired-message capture --------------------------------------------------------

func TestRecordSendAndTakeFired(t *testing.T) {
	ctrl := &controller{}
	ctrl.recordSend("a")
	ctrl.recordSend("b")
	got := ctrl.takeFired()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("expected [a b], got %v", got)
	}
	if got2 := ctrl.takeFired(); len(got2) != 0 {
		t.Fatalf("expected takeFired to clear the buffer, got %v", got2)
	}
}

func TestResetFiredDiscardsPriorCapture(t *testing.T) {
	ctrl := &controller{}
	ctrl.recordSend("old")
	ctrl.resetFired()
	ctrl.recordSend("new")
	got := ctrl.takeFired()
	if len(got) != 1 || got[0] != "new" {
		t.Fatalf("expected only [new], got %v", got)
	}
}

func TestWatchLogsAllFiredEventsInOnePollLine(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "activity.log")
	activity, err := newActivityLogger(logPath, activityLogMaxBytes)
	if err != nil {
		t.Fatalf("newActivityLogger: %v", err)
	}
	defer activity.close()

	// Relative to the seed below, this fires a wicket, a team-50 crossing and
	// a batter-50 milestone all in one poll.
	const body = `{
		"miniscore": {
			"batsmanstriker": {"name": "Rohit Sharma", "runs": 54, "balls": 38},
			"batsmannonstriker": {"name": "KL Rahul", "runs": 0, "balls": 0},
			"lastwkt": "Virat Kohli c Smith b Starc 34(40) - 201/3 in 38.2 ov.",
			"inningsscores": {"inningsscore": [{"inningsid": 1, "batteamshortname": "IND", "runs": 201, "wickets": 3, "overs": 38.2}]},
			"inningsid": 1,
			"batteamscore": {"teamid": 1, "teamscore": 201, "teamwkts": 3}
		},
		"matchheaders": {
			"state": "In Progress",
			"status": "",
			"matchformat": "ODI",
			"team1": {"teamname": "India", "teamsname": "IND"},
			"team2": {"teamname": "Australia", "teamsname": "AUS"}
		}
	}`
	ctrl, _, _ := newTestController(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	})
	ctrl.activity = activity
	ctrl.matchID = 1
	ctrl.prev = ScoreState{
		Valid: true, State: "In Progress",
		Team1Name: "India", Team1Short: "IND", Team2Name: "Australia", Team2Short: "AUS",
		InningsID: 1, BatTeamShort: "IND", Runs: 148, Wickets: 2, Overs: 30.1,
		Striker:    Batter{Name: "Rohit Sharma", Runs: 48, Balls: 35},
		NonStriker: Batter{Name: "Virat Kohli", Runs: 34, Balls: 40},
	}

	ctrl.watch(context.Background())
	activity.close()

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	line := string(logBytes)
	if !strings.Contains(line, "3 event(s):") {
		t.Fatalf("expected a 3-event summary line, got %q", line)
	}
	for _, want := range []string{"Wicket", "reach", "reaches"} {
		if !strings.Contains(line, want) {
			t.Fatalf("expected the log line to mention %q, got %q", want, line)
		}
	}
}

// --- Small wiring helpers ----------------------------------------------------------

func TestEnvDuration(t *testing.T) {
	const key = "TEST_ENV_DURATION_XYZ"
	t.Run("unset uses default", func(t *testing.T) {
		os.Unsetenv(key)
		if got := envDuration(key, 7*time.Second); got != 7*time.Second {
			t.Fatalf("expected default, got %v", got)
		}
	})
	t.Run("valid duration parsed", func(t *testing.T) {
		os.Setenv(key, "45s")
		defer os.Unsetenv(key)
		if got := envDuration(key, 7*time.Second); got != 45*time.Second {
			t.Fatalf("expected 45s, got %v", got)
		}
	})
	t.Run("invalid falls back to default without panic", func(t *testing.T) {
		os.Setenv(key, "nonsense")
		defer os.Unsetenv(key)
		if got := envDuration(key, 7*time.Second); got != 7*time.Second {
			t.Fatalf("expected default on invalid input, got %v", got)
		}
	})
}

func TestRunEveryCallsImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := make(chan struct{}, 1)
	go runEvery(ctx, time.Hour, func(context.Context) {
		select {
		case called <- struct{}{}:
		default:
		}
	})
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatalf("expected fn to be called immediately, before any tick")
	}
}

func TestRunEveryReturnsPromptlyOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before runEvery even starts
	var calls int32
	done := make(chan struct{})
	go func() {
		// A 1-hour interval means a second call is only possible via a real
		// tick, which can't happen inside this test's timeout — so if calls
		// ends up 1 once done closes, both "returns promptly" and "does not
		// tick again" are proven together.
		runEvery(ctx, time.Hour, func(context.Context) { atomic.AddInt32(&calls, 1) })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("runEvery did not return promptly for an already-cancelled context")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 call (the immediate one), got %d", got)
	}
}

func TestLogActivityErrorRoutesAPIError(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "activity.log")
	activity, err := newActivityLogger(logPath, activityLogMaxBytes)
	if err != nil {
		t.Fatalf("newActivityLogger: %v", err)
	}
	defer activity.close()
	ctrl := &controller{activity: activity}

	ctrl.logActivityError("watch", &apiError{path: "/x", status: 429, body: []byte("slow down")})
	ctrl.logActivityError("discovery", errors.New("boom"))
	activity.close()

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), got)
	}
	if !strings.Contains(lines[0], "HTTP 429") || !strings.Contains(lines[0], "slow down") {
		t.Fatalf("expected an *apiError to preserve status and body via logAPIError, got %q", lines[0])
	}
	if !strings.Contains(lines[1], "boom") || strings.Contains(lines[1], "HTTP") {
		t.Fatalf("expected a non-apiError to route to logError, got %q", lines[1])
	}
}

// --- Concurrency -----------------------------------------------------------------

func TestConcurrentDiscoverAndWatch(t *testing.T) {
	indiaBody := readFixture(t, "live_matches_india.json")
	scoreBody := readFixture(t, "leanback_test_day3.json")
	completeBody := readFixture(t, "leanback_complete.json")

	var counter int32
	ctrl, _, _ := newTestController(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&counter, 1)
		switch {
		case strings.Contains(r.URL.Path, "/matches/v1/live"):
			w.Write(indiaBody)
		case n%2 == 0:
			w.Write(completeBody)
		default:
			w.Write(scoreBody)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500 && ctx.Err() == nil; i++ {
			ctrl.discover(ctx)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500 && ctx.Err() == nil; i++ {
			ctrl.watch(ctx)
		}
	}()
	wg.Wait()
	// No assertion beyond "no race, no panic" — that is the point.
}
