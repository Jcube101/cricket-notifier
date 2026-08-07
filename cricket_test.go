package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// readFixture loads a captured/derived API response from testdata/. See
// testdata/README.md for provenance.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func decodeLeanback(t *testing.T, name string) leanbackResponse {
	t.Helper()
	var lb leanbackResponse
	if err := json.Unmarshal(readFixture(t, name), &lb); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return lb
}

// newTestClient builds a CricketClient pointed at an httptest server via the
// baseURL seam, so no test ever reaches the real RapidAPI.
func newTestClient(srv *httptest.Server) *CricketClient {
	return &CricketClient{apiKey: "test-key", baseURL: srv.URL, http: srv.Client()}
}

// --- toScoreState — JSON to snapshot ----------------------------------------

func TestToScoreStateDay3Fixture(t *testing.T) {
	lb := decodeLeanback(t, "leanback_test_day3.json")
	got := lb.toScoreState(152496)
	want := ScoreState{
		MatchID:      152496,
		Format:       "TEST",
		State:        "Delay",
		Status:       "Day 3: 3rd Session - West Indies lead by 107 runs",
		Team1Name:    "West Indies",
		Team1Short:   "WI",
		Team2Name:    "Pakistan",
		Team2Short:   "PAK",
		InningsID:    3,
		BatTeamShort: "WI",
		Runs:         78,
		Wickets:      4,
		Overs:        26.2,
		Striker:      Batter{Name: "Tagenarine Chanderpaul", Runs: 35, Balls: 92},
		NonStriker:   Batter{Name: "Justin Greaves", Runs: 17, Balls: 24},
		LastWkt:      "Shai Hope   b Khurram Shahzad 10(8)  - 43/4 in 16.2 ov.",
		Valid:        true,
	}
	if got != want {
		t.Fatalf("toScoreState mismatch:\n got  %+v\nwant  %+v", got, want)
	}
}

func TestToScoreStateIgnoresUnknownMiniscoreFields(t *testing.T) {
	// leanback_test_day3.json's miniscore object carries 25 keys (crr, udrs,
	// partnership, performance, bowlerstriker, ...); toScoreState only reads 5
	// of them. This proves the rest are safely ignored rather than tripping a
	// decode error — the struct tolerates a payload far wider than it models.
	lb := decodeLeanback(t, "leanback_test_day3.json")
	got := lb.toScoreState(152496)
	if !got.Valid || got.Runs != 78 || got.Striker.Name != "Tagenarine Chanderpaul" {
		t.Fatalf("expected a clean decode ignoring extra fields, got %+v", got)
	}
}

func TestToScoreStateNullMiniscore(t *testing.T) {
	lb := decodeLeanback(t, "leanback_null_miniscore.json")
	got := lb.toScoreState(152496)
	if !got.Valid {
		t.Fatalf("expected Valid true even with a nil miniscore, got false")
	}
	if got.Runs != 0 || got.Wickets != 0 || got.InningsID != 0 {
		t.Fatalf("expected zero score fields with nil miniscore, got %+v", got)
	}
	if got.State != "Preview" {
		t.Fatalf("expected state %q, got %q", "Preview", got.State)
	}
}

func TestToScoreStateNoCurrentInnings(t *testing.T) {
	lb := decodeLeanback(t, "leanback_no_current_innings.json")
	got := lb.toScoreState(152496)
	if got.Runs != 78 || got.Wickets != 4 {
		t.Fatalf("expected fallback to batteamscore 78/4, got %d/%d", got.Runs, got.Wickets)
	}
	if got.Overs != 0 {
		t.Fatalf("expected Overs to stay zero without an innings line, got %v", got.Overs)
	}
	if got.BatTeamShort != "" {
		t.Fatalf("expected BatTeamShort empty without an innings line, got %q", got.BatTeamShort)
	}
	if name := got.BatTeamName(); name != "" {
		t.Fatalf("expected BatTeamName to degrade to the empty short name, got %q", name)
	}
}

func TestToScoreStateInningsLineWinsOverBatTeamScore(t *testing.T) {
	lb := decodeLeanback(t, "leanback_score_disagrees.json")
	got := lb.toScoreState(152496)
	if got.Runs != 78 || got.Wickets != 4 {
		t.Fatalf("expected the innings line (78/4) to win over batteamscore (999/9), got %d/%d", got.Runs, got.Wickets)
	}
}

func TestToScoreStateEmptyMatchHeaders(t *testing.T) {
	var lb leanbackResponse
	got := lb.toScoreState(1)
	if want := " vs "; got.Title() != want {
		t.Fatalf("expected Title() %q with empty matchheaders, got %q", want, got.Title())
	}
}

// --- currentInnings ----------------------------------------------------------

func TestCurrentInnings(t *testing.T) {
	lb := decodeLeanback(t, "leanback_test_day3.json")
	cur := lb.Miniscore.currentInnings()
	if cur == nil || cur.InningsID != 3 || cur.Runs != 78 {
		t.Fatalf("expected the innings-3 line (78 runs), got %+v", cur)
	}
}

func TestCurrentInningsNoMatch(t *testing.T) {
	lb := decodeLeanback(t, "leanback_no_current_innings.json")
	if cur := lb.Miniscore.currentInnings(); cur != nil {
		t.Fatalf("expected nil when no innings line matches the active innings id, got %+v", cur)
	}
}

func TestCurrentInningsNotFirstInArray(t *testing.T) {
	// Real responses arrive newest-first, so the active innings happens to sit
	// at index 0 in leanback_test_day3.json. This fixture re-sorts the array
	// ascending so the active innings (3) is last, catching a naive
	// "take the first line" implementation that would pass by luck otherwise.
	lb := decodeLeanback(t, "leanback_innings_current_not_first.json")
	cur := lb.Miniscore.currentInnings()
	if cur == nil || cur.InningsID != 3 || cur.Runs != 78 {
		t.Fatalf("expected to find innings 3 even though it's last in the array, got %+v", cur)
	}
}

// --- fetchLiveIndiaMatch ------------------------------------------------------

func TestFetchLiveIndiaMatchFound(t *testing.T) {
	body := readFixture(t, "live_matches_india.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-requests-remaining", "150")
		w.Write(body)
	}))
	defer srv.Close()
	c := newTestClient(srv)
	match, remaining, err := c.fetchLiveIndiaMatch(context.Background(), newDisabledActivityLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match == nil || match.MatchID != 152496 {
		t.Fatalf("expected match 152496, got %+v", match)
	}
	if remaining != 150 {
		t.Fatalf("expected remaining 150, got %d", remaining)
	}
}

func TestFetchLiveIndiaMatchNoIndia(t *testing.T) {
	body := readFixture(t, "live_matches_no_india.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()
	c := newTestClient(srv)
	match, _, err := c.fetchLiveIndiaMatch(context.Background(), newDisabledActivityLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match != nil {
		t.Fatalf("expected nil match (not an error) when no India match is live, got %+v", match)
	}
}

func TestFetchLiveIndiaMatchNullWrapper(t *testing.T) {
	body := readFixture(t, "live_matches_null_wrapper.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()
	c := newTestClient(srv)
	match, _, err := c.fetchLiveIndiaMatch(context.Background(), newDisabledActivityLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match == nil || match.MatchID != 152496 {
		t.Fatalf("expected match 152496 found behind the two malformed series, got %+v", match)
	}
}

func TestFetchLiveIndiaMatchTerminalExcluded(t *testing.T) {
	body := readFixture(t, "live_matches_india_complete.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()
	c := newTestClient(srv)
	match, _, err := c.fetchLiveIndiaMatch(context.Background(), newDisabledActivityLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match != nil {
		t.Fatalf("expected nil for a terminal (Complete) India match, got %+v", match)
	}
}

func TestFetchLiveIndiaMatchAWomenExcluded(t *testing.T) {
	body := readFixture(t, "live_matches_india_a_women.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()
	c := newTestClient(srv)
	match, _, err := c.fetchLiveIndiaMatch(context.Background(), newDisabledActivityLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match != nil {
		t.Fatalf("expected nil: India A and India Women are both live but neither counts, got %+v", match)
	}
}

func TestFetchLiveIndiaMatchFirstWatchableWins(t *testing.T) {
	// No captured fixture has two live India matches at once; this minimal
	// synthetic body isolates the loop's "first match wins" behaviour rather
	// than testing JSON shape, which the fixture-driven tests above already cover.
	const body = `{
		"typeMatches": [{
			"seriesMatches": [
				{"seriesAdWrapper": {"matches": [
					{"matchInfo": {"matchId": 1001, "state": "In Progress",
						"team1": {"teamName": "India", "teamSName": "IND"},
						"team2": {"teamName": "England", "teamSName": "ENG"}}}
				]}},
				{"seriesAdWrapper": {"matches": [
					{"matchInfo": {"matchId": 1002, "state": "In Progress",
						"team1": {"teamName": "India", "teamSName": "IND"},
						"team2": {"teamName": "Australia", "teamSName": "AUS"}}}
				]}}
			]
		}]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()
	c := newTestClient(srv)
	match, _, err := c.fetchLiveIndiaMatch(context.Background(), newDisabledActivityLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match == nil || match.MatchID != 1001 {
		t.Fatalf("expected the first watchable match (1001) to win, got %+v", match)
	}
}

func TestFetchLiveIndiaMatchSkipsWarmupFindsRealMatch(t *testing.T) {
	body := readFixture(t, "live_matches_india_warmup_then_real.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()
	c := newTestClient(srv)
	match, _, err := c.fetchLiveIndiaMatch(context.Background(), newDisabledActivityLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match == nil || match.MatchID != 169500 {
		t.Fatalf("expected the warm-up match (169497) skipped and the real Test (169500) returned, got %+v", match)
	}
}

func TestIsExhibitionMatch(t *testing.T) {
	cases := []struct {
		desc string
		want bool
	}{
		{"3-Day Warm-up Match", true},
		{"3 -Day Warm-up match", true},
		{"WARM-UP MATCH", true},
		{"Practice Match", true},
		{"Tour Match", true},
		{"1st Test", false},
		{"2nd ODI", false},
		{"3rd T20I", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isExhibitionMatch(c.desc); got != c.want {
			t.Errorf("isExhibitionMatch(%q) = %v, want %v", c.desc, got, c.want)
		}
	}
}

func TestFetchLiveIndiaMatchMalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-requests-remaining", "99")
		w.Write([]byte("{not valid json"))
	}))
	defer srv.Close()
	c := newTestClient(srv)
	match, remaining, err := c.fetchLiveIndiaMatch(context.Background(), newDisabledActivityLogger())
	if err == nil {
		t.Fatalf("expected a decode error for a malformed body")
	}
	if match != nil {
		t.Fatalf("expected nil match on decode error, got %+v", match)
	}
	if remaining != 99 {
		t.Fatalf("expected quota still reported (99) despite the decode error, got %d", remaining)
	}
}

// --- get: headers and errors --------------------------------------------------

func TestGetQuotaHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-requests-remaining", "42")
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	c := newTestClient(srv)
	_, remaining, err := c.get(context.Background(), "/x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if remaining != 42 {
		t.Fatalf("expected remaining 42, got %d", remaining)
	}
}

func TestGetQuotaHeaderAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	c := newTestClient(srv)
	_, remaining, err := c.get(context.Background(), "/x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if remaining != -1 {
		t.Fatalf("expected remaining -1 when the header is absent, got %d", remaining)
	}
}

func TestGetQuotaHeaderNonNumeric(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-requests-remaining", "lots")
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	c := newTestClient(srv)
	_, remaining, err := c.get(context.Background(), "/x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if remaining != -1 {
		t.Fatalf("expected remaining -1 for a non-numeric header (not 0, which would trip the quota guard), got %d", remaining)
	}
}

func TestGetSendsRapidAPIHeaders(t *testing.T) {
	var gotKey, gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-RapidAPI-Key")
		gotHost = r.Header.Get("X-RapidAPI-Host")
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	c := &CricketClient{apiKey: "secret-key", baseURL: srv.URL, http: srv.Client()}
	if _, _, err := c.get(context.Background(), "/x"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotKey != "secret-key" {
		t.Fatalf("expected X-RapidAPI-Key %q, got %q", "secret-key", gotKey)
	}
	if gotHost != apiHost {
		t.Fatalf("expected X-RapidAPI-Host %q, got %q", apiHost, gotHost)
	}
}

func TestGetNon200ReturnsAPIError(t *testing.T) {
	for _, status := range []int{429, 500} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("x-ratelimit-requests-remaining", "5")
				w.WriteHeader(status)
				w.Write([]byte("rate limited"))
			}))
			defer srv.Close()
			c := newTestClient(srv)
			_, remaining, err := c.get(context.Background(), "/x")
			var ae *apiError
			if !errors.As(err, &ae) {
				t.Fatalf("expected a *apiError, got %T: %v", err, err)
			}
			if ae.status != status {
				t.Fatalf("expected status %d, got %d", status, ae.status)
			}
			if string(ae.body) != "rate limited" {
				t.Fatalf("expected body %q, got %q", "rate limited", ae.body)
			}
			if remaining != 5 {
				t.Fatalf("expected quota still reported (5) alongside the error, got %d", remaining)
			}
		})
	}
}

func TestAPIErrorString(t *testing.T) {
	ae := &apiError{path: "/matches/v1/live", status: 429}
	want := "/matches/v1/live returned status 429"
	if got := ae.Error(); got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestGetCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	c := newTestClient(srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.get(ctx, "/x"); err == nil {
		t.Fatalf("expected an error from an already-cancelled context")
	}
}

// --- team filters --------------------------------------------------------------

func TestIsIndia(t *testing.T) {
	tests := []struct {
		name string
		team team
		want bool
	}{
		{"senior men by short name", team{TeamSName: "IND"}, true},
		{"senior men by full name", team{TeamName: "India"}, true},
		{"india a excluded", team{TeamSName: "INDA", TeamName: "India A"}, false},
		{"india women excluded", team{TeamSName: "INDW", TeamName: "India Women"}, false},
		{"other team", team{TeamSName: "AUS", TeamName: "Australia"}, false},
		{"zero value", team{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isIndia(tt.team); got != tt.want {
				t.Fatalf("isIndia(%+v) = %v, want %v", tt.team, got, tt.want)
			}
		})
	}
}

func TestInvolvesIndia(t *testing.T) {
	india := team{TeamSName: "IND"}
	other := team{TeamSName: "AUS"}
	tests := []struct {
		name string
		mi   matchInfo
		want bool
	}{
		{"team1 is india", matchInfo{Team1: india, Team2: other}, true},
		{"team2 is india", matchInfo{Team1: other, Team2: india}, true},
		{"neither", matchInfo{Team1: other, Team2: other}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := involvesIndia(tt.mi); got != tt.want {
				t.Fatalf("involvesIndia(%+v) = %v, want %v", tt.mi, got, tt.want)
			}
		})
	}
}

func TestIsWatchable(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{"", false},
		{"Complete", false},
		{"Abandoned", false},
		{"No result", false},
		{"  complete  ", false},
		{"ABANDONED", false},
		{"In Progress", true},
		{"Innings Break", true},
		{"Toss", true},
		{"Preview", true},
		{"Delay", true},
		{"Stumps", true},
		{"Rain", true},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			if got := isWatchable(tt.state); got != tt.want {
				t.Fatalf("isWatchable(%q) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

// --- fetchMatchScore -----------------------------------------------------------

func TestFetchMatchScoreHappyPath(t *testing.T) {
	body := readFixture(t, "leanback_test_day3.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()
	c := newTestClient(srv)
	got, _, err := c.fetchMatchScore(context.Background(), 152496)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.MatchID != 152496 {
		t.Fatalf("expected MatchID 152496, got %d", got.MatchID)
	}
	if !got.Valid || got.Runs != 78 {
		t.Fatalf("expected a populated ScoreState, got %+v", got)
	}
}

func TestFetchMatchScoreNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := newTestClient(srv)
	got, _, err := c.fetchMatchScore(context.Background(), 152496)
	if err == nil {
		t.Fatalf("expected an error on a non-200 response")
	}
	if got != (ScoreState{}) {
		t.Fatalf("expected a zero ScoreState (Valid false) on a failed fetch, got %+v", got)
	}
}
