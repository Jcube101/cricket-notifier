package main

import (
	"strings"
	"testing"
)

// newTestNotifier returns a Notifier whose messages are captured into a slice
// instead of being sent to Telegram, plus a pointer to that slice.
func newTestNotifier() (*Notifier, *[]string) {
	var sent []string
	n := &Notifier{send: func(text string) error {
		sent = append(sent, text)
		return nil
	}}
	return n, &sent
}

// base is a reasonable mid-innings snapshot to mutate in each test.
func base() ScoreState {
	return ScoreState{
		MatchID:   1,
		State:     "In Progress",
		Team1Name: "India", Team1Short: "IND",
		Team2Name: "Australia", Team2Short: "AUS",
		InningsID:    1,
		BatTeamShort: "IND",
		Runs:         148,
		Wickets:      2,
		Overs:        30.1,
		Striker:      Batter{Name: "Rohit Sharma", Runs: 48, Balls: 35},
		NonStriker:   Batter{Name: "Virat Kohli", Runs: 34, Balls: 40},
		Valid:        true,
	}
}

func wantOne(t *testing.T, sent []string, substr string) {
	t.Helper()
	if len(sent) != 1 {
		t.Fatalf("want exactly 1 message, got %d: %v", len(sent), sent)
	}
	if !strings.Contains(sent[0], substr) {
		t.Fatalf("message %q does not contain %q", sent[0], substr)
	}
}

func TestMatchStart(t *testing.T) {
	n, sent := newTestNotifier()
	prev := base()
	prev.State = "Preview"
	prev.InningsID = 0
	curr := base()
	n.checkAndNotify(prev, curr)
	wantOne(t, *sent, "India vs Australia — Match started")
}

func TestWicket(t *testing.T) {
	n, sent := newTestNotifier()
	prev := base()
	curr := base()
	curr.Wickets = 3
	// runs unchanged (148) so no team-50 milestone also fires
	curr.LastWkt = "Virat Kohli c Smith b Starc 34(40)  - 148/3 in 30.1 ov."
	curr.NonStriker = Batter{Name: "KL Rahul", Runs: 0, Balls: 0}
	n.checkAndNotify(prev, curr)
	wantOne(t, *sent, "💥 Wicket! Virat Kohli out for 34. India 148/3")
}

func TestTeamMilestone(t *testing.T) {
	n, sent := newTestNotifier()
	prev := base()
	curr := base()
	curr.Runs = 201
	// wickets unchanged (2) so no wicket event also fires
	curr.Overs = 38.2
	// nudge striker so no accidental batter milestone here
	curr.Striker = Batter{Name: "Rohit Sharma", Runs: 49, Balls: 36}
	n.checkAndNotify(prev, curr)
	wantOne(t, *sent, "📊 India reach 200 (201/2 in 38.2 overs)")
}

func TestBatterMilestone(t *testing.T) {
	n, sent := newTestNotifier()
	prev := base()
	curr := base()
	curr.Striker = Batter{Name: "Rohit Sharma", Runs: 54, Balls: 38}
	curr.Runs = 149 // stays in the same 50-bucket as prev (148) so no team milestone
	n.checkAndNotify(prev, curr)
	wantOne(t, *sent, "🎉 Rohit Sharma reaches 50! (54 off 38)")
}

func TestInningsChange(t *testing.T) {
	n, sent := newTestNotifier()
	prev := base()
	prev.Runs = 287
	prev.Wickets = 6
	curr := base()
	curr.InningsID = 2
	curr.BatTeamShort = "AUS"
	curr.Runs = 12
	curr.Wickets = 0
	n.checkAndNotify(prev, curr)
	wantOne(t, *sent, "🔄 End of 1st innings. India 287/6")
}

func TestMatchResult(t *testing.T) {
	n, sent := newTestNotifier()
	prev := base()
	curr := base()
	curr.State = "Complete"
	curr.Status = "India won by 45 runs"
	n.checkAndNotify(prev, curr)
	wantOne(t, *sent, "🏆 Match result: India won by 45 runs")
}

func TestNoChangeNoMessage(t *testing.T) {
	n, sent := newTestNotifier()
	curr := base()
	n.checkAndNotify(curr, curr)
	if len(*sent) != 0 {
		t.Fatalf("expected no messages on identical snapshots, got %v", *sent)
	}
}
