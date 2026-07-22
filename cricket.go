package main

// cricket.go — talks to the (unofficial) Cricbuzz API exposed through RapidAPI.
//
// Two endpoints are used:
//
//   GET /matches/v1/live              -> list of currently relevant matches
//   GET /mcenter/v1/{matchId}/leanback -> live "mini score" for one match
//
// The leanback endpoint is the workhorse: a single call gives us the match
// state, the team score, the wickets, the current innings and both batters,
// which between them are enough to detect all six notification events.
//
// Every RapidAPI response carries an "x-ratelimit-requests-remaining" header
// telling us how much of the monthly quota is left. We surface that value so
// the caller can slow down or stop before the quota (200/month) runs out.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	apiHost    = "cricbuzz-cricket.p.rapidapi.com"
	apiBaseURL = "https://" + apiHost
)

// CricketClient holds the API key and a reusable HTTP client. Everything that
// talks to Cricbuzz hangs off this so the key lives in exactly one place.
type CricketClient struct {
	apiKey string
	http   *http.Client
}

// NewCricketClient builds a client with a sensible request timeout.
func NewCricketClient(apiKey string) *CricketClient {
	return &CricketClient{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 20 * time.Second},
	}
}

// get performs a GET against the API and returns the raw body plus the number
// of monthly requests still remaining (-1 if the header was missing). The
// context lets an in-flight request be cancelled during shutdown.
func (c *CricketClient) get(ctx context.Context, path string) (body []byte, remaining int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBaseURL+path, nil)
	if err != nil {
		return nil, -1, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-RapidAPI-Key", c.apiKey)
	req.Header.Set("X-RapidAPI-Host", apiHost)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, -1, fmt.Errorf("request %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, -1, fmt.Errorf("read body: %w", err)
	}

	remaining = -1
	if h := resp.Header.Get("x-ratelimit-requests-remaining"); h != "" {
		if n, convErr := strconv.Atoi(h); convErr == nil {
			remaining = n
		}
	}

	if resp.StatusCode != http.StatusOK {
		return body, remaining, &apiError{path: path, status: resp.StatusCode, body: body}
	}
	return body, remaining, nil
}

// apiError is returned by get on a non-200 response. It carries the HTTP status
// and raw body so the caller can log them; its Error string is unchanged from
// the previous fmt.Errorf form so existing log output stays identical.
type apiError struct {
	path   string
	status int
	body   []byte
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s returned status %d", e.path, e.status)
}

// --- /matches/v1/live response model ----------------------------------------

type liveMatchesResponse struct {
	TypeMatches []struct {
		SeriesMatches []struct {
			SeriesAdWrapper *struct {
				Matches []struct {
					MatchInfo matchInfo `json:"matchInfo"`
				} `json:"matches"`
			} `json:"seriesAdWrapper"`
		} `json:"seriesMatches"`
	} `json:"typeMatches"`
}

type matchInfo struct {
	MatchID     int    `json:"matchId"`
	SeriesName  string `json:"seriesName"`
	MatchDesc   string `json:"matchDesc"`
	MatchFormat string `json:"matchFormat"`
	State       string `json:"state"`
	Status      string `json:"status"`
	Team1       team   `json:"team1"`
	Team2       team   `json:"team2"`
}

type team struct {
	TeamName  string `json:"teamName"`
	TeamSName string `json:"teamSName"`
}

// fetchLiveIndiaMatch returns the first currently-live match that India is
// playing in, or (nil, nil) when there is no such match.
func (c *CricketClient) fetchLiveIndiaMatch(ctx context.Context) (*matchInfo, int, error) {
	body, remaining, err := c.get(ctx, "/matches/v1/live")
	if err != nil {
		return nil, remaining, err
	}

	var data liveMatchesResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, remaining, fmt.Errorf("decode live matches: %w", err)
	}

	for _, tm := range data.TypeMatches {
		for _, sm := range tm.SeriesMatches {
			if sm.SeriesAdWrapper == nil {
				continue
			}
			for _, m := range sm.SeriesAdWrapper.Matches {
				mi := m.MatchInfo
				if involvesIndia(mi) && isWatchable(mi.State) {
					found := mi // copy so we don't return a loop pointer
					return &found, remaining, nil
				}
			}
		}
	}
	return nil, remaining, nil
}

// involvesIndia reports whether the senior India team is playing in this match.
// Broaden the check here (e.g. "India A", "India Women") if you want those too.
func involvesIndia(mi matchInfo) bool {
	return isIndia(mi.Team1) || isIndia(mi.Team2)
}

func isIndia(t team) bool {
	return t.TeamSName == "IND" || t.TeamName == "India"
}

// isWatchable filters out matches that are over (or never happened). Anything
// else — in progress, an innings/rain break, toss, preview — is worth polling.
func isWatchable(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", "complete", "abandoned", "no result":
		return false
	}
	return true
}

// --- ScoreState: the flattened snapshot we compare between polls ------------

// Batter is one active batter's contribution, used for milestone detection.
type Batter struct {
	Name  string
	Runs  int
	Balls int
}

// ScoreState is a clean, comparison-friendly snapshot of a match at one moment.
// fetchMatchScore returns it; checkAndNotify (notifier.go) diffs two of them.
// It deliberately hides the messy raw API shape behind plain fields.
type ScoreState struct {
	MatchID int
	Format  string // "ODI", "T20", "TEST"
	State   string // "Preview", "In Progress", "Innings Break", "Complete", ...
	Status  string // human-readable status line from the API

	Team1Name, Team1Short string
	Team2Name, Team2Short string

	InningsID    int    // id of the innings currently in play
	BatTeamShort string // short name of the batting team, e.g. "IND"
	Runs         int
	Wickets      int
	Overs        float64

	Striker    Batter
	NonStriker Batter

	LastWkt string // raw "last wicket" description, used to name a dismissed batter

	Valid bool // false for the zero value; true once populated from the API
}

// BatTeamName resolves the batting team's full name from its short name.
func (s ScoreState) BatTeamName() string {
	switch s.BatTeamShort {
	case s.Team1Short:
		return s.Team1Name
	case s.Team2Short:
		return s.Team2Name
	default:
		return s.BatTeamShort
	}
}

// Title is the "India vs Australia" headline used in messages.
func (s ScoreState) Title() string {
	return s.Team1Name + " vs " + s.Team2Name
}

// --- /mcenter/v1/{id}/leanback response model -------------------------------

type leanbackResponse struct {
	Miniscore    *miniscore   `json:"miniscore"`
	MatchHeaders matchHeaders `json:"matchheaders"`
}

type matchHeaders struct {
	State       string `json:"state"`
	Status      string `json:"status"`
	MatchFormat string `json:"matchformat"`
	MatchDesc   string `json:"matchdesc"`
	Team1       lbTeam `json:"team1"`
	Team2       lbTeam `json:"team2"`
}

type lbTeam struct {
	TeamID    int    `json:"teamid"`
	TeamName  string `json:"teamname"`
	TeamSName string `json:"teamsname"`
}

type miniscore struct {
	InningsID         int          `json:"inningsid"`
	BatsmanStriker    lbBatsman    `json:"batsmanstriker"`
	BatsmanNonStriker lbBatsman    `json:"batsmannonstriker"`
	BatTeamScore      batTeamScore `json:"batteamscore"`
	LastWkt           string       `json:"lastwkt"` // pre-formatted, e.g. "Kohli b Starc 34(40) - 187/3 in 30.1 ov."
	InningsScores     struct {
		InningsScore []inningsScore `json:"inningsscore"`
	} `json:"inningsscores"`
}

type lbBatsman struct {
	Name  string `json:"name"`
	Runs  int    `json:"runs"`
	Balls int    `json:"balls"`
}

type batTeamScore struct {
	TeamID    int `json:"teamid"`
	TeamScore int `json:"teamscore"`
	TeamWkts  int `json:"teamwkts"`
}

type inningsScore struct {
	InningsID        int     `json:"inningsid"`
	BatTeamShortName string  `json:"batteamshortname"`
	Runs             int     `json:"runs"`
	Wickets          int     `json:"wickets"`
	Overs            float64 `json:"overs"`
}

// fetchMatchScore pulls the live mini-score for one match and flattens it into
// the ScoreState we diff against in the notifier.
func (c *CricketClient) fetchMatchScore(ctx context.Context, matchID int) (ScoreState, int, error) {
	body, remaining, err := c.get(ctx, fmt.Sprintf("/mcenter/v1/%d/leanback", matchID))
	if err != nil {
		return ScoreState{}, remaining, err
	}

	var lb leanbackResponse
	if err := json.Unmarshal(body, &lb); err != nil {
		return ScoreState{}, remaining, fmt.Errorf("decode leanback: %w", err)
	}
	return lb.toScoreState(matchID), remaining, nil
}

// toScoreState maps the raw leanback payload onto our flat snapshot struct.
func (lb leanbackResponse) toScoreState(matchID int) ScoreState {
	h := lb.MatchHeaders
	s := ScoreState{
		MatchID:    matchID,
		Format:     h.MatchFormat,
		State:      h.State,
		Status:     h.Status,
		Team1Name:  h.Team1.TeamName,
		Team1Short: h.Team1.TeamSName,
		Team2Name:  h.Team2.TeamName,
		Team2Short: h.Team2.TeamSName,
		Valid:      true,
	}

	if lb.Miniscore != nil {
		m := lb.Miniscore
		s.InningsID = m.InningsID
		s.Striker = Batter{Name: m.BatsmanStriker.Name, Runs: m.BatsmanStriker.Runs, Balls: m.BatsmanStriker.Balls}
		s.NonStriker = Batter{Name: m.BatsmanNonStriker.Name, Runs: m.BatsmanNonStriker.Runs, Balls: m.BatsmanNonStriker.Balls}
		s.LastWkt = m.LastWkt

		// Prefer the per-innings line that matches the active innings; it carries
		// the batting team's short name and over count alongside the score.
		if cur := m.currentInnings(); cur != nil {
			s.Runs = cur.Runs
			s.Wickets = cur.Wickets
			s.Overs = cur.Overs
			s.BatTeamShort = cur.BatTeamShortName
		} else {
			s.Runs = m.BatTeamScore.TeamScore
			s.Wickets = m.BatTeamScore.TeamWkts
		}
	}
	return s
}

// currentInnings returns the innings line for the innings in play, or nil.
func (m *miniscore) currentInnings() *inningsScore {
	for i := range m.InningsScores.InningsScore {
		if m.InningsScores.InningsScore[i].InningsID == m.InningsID {
			return &m.InningsScores.InningsScore[i]
		}
	}
	return nil
}
