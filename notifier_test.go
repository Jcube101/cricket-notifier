package main

import (
	"errors"
	"fmt"
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

// wantAll asserts sent contains exactly len(substrs) messages, each containing
// the corresponding substring in order.
func wantAll(t *testing.T, sent []string, substrs ...string) {
	t.Helper()
	if len(sent) != len(substrs) {
		t.Fatalf("want exactly %d messages, got %d: %v", len(substrs), len(sent), sent)
	}
	for i, substr := range substrs {
		if !strings.Contains(sent[i], substr) {
			t.Fatalf("message %d %q does not contain %q", i, sent[i], substr)
		}
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

// --- Multi-event polls -------------------------------------------------------

func TestMultiEventPollFiresInOrder(t *testing.T) {
	n, sent := newTestNotifier()
	prev := base() // Runs 148, Wickets 2, Striker Rohit Sharma 48, NonStriker Virat Kohli 34
	curr := base()
	curr.Wickets = 3 // wicket event
	curr.Runs = 201  // crosses 200 -- team milestone
	curr.Overs = 38.2
	curr.Striker = Batter{Name: "Rohit Sharma", Runs: 54, Balls: 40} // crosses 50 -- batter milestone
	curr.NonStriker = Batter{Name: "KL Rahul", Runs: 0, Balls: 0}    // Kohli got out; new batter, no baseline
	curr.LastWkt = "Virat Kohli c Smith b Starc 34(40) - 201/3 in 38.2 ov."
	n.checkAndNotify(prev, curr)
	wantAll(t, *sent,
		"💥 Wicket! Virat Kohli out for 34. India 201/3",
		"📊 India reach 200 (201/3 in 38.2 overs)",
		"🎉 Rohit Sharma reaches 50! (54 off 40)",
	)
}

func TestMatchStartSuppressesOtherEvents(t *testing.T) {
	n, sent := newTestNotifier()
	prev := ScoreState{
		State: "Preview", Valid: true,
		Team1Name: "India", Team1Short: "IND",
		Team2Name: "Australia", Team2Short: "AUS",
		InningsID: 0,
	}
	curr := base() // InningsID 1, Runs 148, Wickets 2 -- would trip a wicket + team milestone unsuppressed
	n.checkAndNotify(prev, curr)
	wantOne(t, *sent, "Match started")
}

func TestMatchStartRainDelay(t *testing.T) {
	// The Step 0b(a) truth table: the innings guard (0 -> non-zero), not the
	// state transition alone, is what distinguishes a genuine start from
	// Cricbuzz reusing "Delay" for both a pre-match rain delay and a mid-match
	// interruption. See LEARNINGS.md.
	tests := []struct {
		name          string
		prevState     string
		prevInningsID int
		currState     string
		currInningsID int
		wantMessage   bool
	}{
		{"preview to delay, still innings 0", "Preview", 0, "Delay", 0, false},
		{"delay to in progress, innings starts", "Delay", 0, "In Progress", 1, true},
		{"delay to in progress mid-match, same innings", "Delay", 3, "In Progress", 3, false},
		{"preview to in progress, innings starts", "Preview", 0, "In Progress", 1, true},
		{"innings break to in progress, innings-change path", "Innings Break", 2, "In Progress", 3, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, sent := newTestNotifier()
			prev := base()
			prev.State = tt.prevState
			prev.InningsID = tt.prevInningsID
			curr := base()
			curr.State = tt.currState
			curr.InningsID = tt.currInningsID
			n.checkAndNotify(prev, curr)
			gotMessage := false
			for _, m := range *sent {
				if strings.Contains(m, "Match started") {
					gotMessage = true
				}
			}
			if gotMessage != tt.wantMessage {
				t.Fatalf("Match started fired=%v, want %v (sent=%v)", gotMessage, tt.wantMessage, *sent)
			}
		})
	}
}

func TestIsPreMatchAndIsTerminalTable(t *testing.T) {
	// Covers every attested state (In Progress, Delay, Innings Break, Complete,
	// Preview, Toss) plus the newly added Rain/Wet Outfield/Inspection, and
	// pins that mid-match break states (Stumps, Lunch, Tea, Drinks) are NOT
	// pre-match -- adding them would resurrect the day-3 rain-delay bug.
	tests := []struct {
		state        string
		wantPreMatch bool
		wantTerminal bool
	}{
		{"", true, false},
		{"In Progress", false, false},
		{"in progress", false, false},
		{"  In Progress  ", false, false},
		{"Delay", true, false},
		{"DELAY", true, false},
		{"Innings Break", false, false},
		{"Complete", false, true},
		{"COMPLETE", false, true},
		{"  complete  ", false, true},
		{"Abandoned", false, true},
		{"No result", false, true},
		{"Preview", true, false},
		{"Toss", true, false},
		{"Rain", true, false},
		{"Wet Outfield", true, false},
		{"Inspection", true, false},
		{"Stumps", false, false},
		{"Lunch", false, false},
		{"Tea", false, false},
		{"Drinks", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			if got := isPreMatch(tt.state); got != tt.wantPreMatch {
				t.Fatalf("isPreMatch(%q) = %v, want %v", tt.state, got, tt.wantPreMatch)
			}
			if got := isTerminal(tt.state); got != tt.wantTerminal {
				t.Fatalf("isTerminal(%q) = %v, want %v", tt.state, got, tt.wantTerminal)
			}
		})
	}
}

// --- Early returns -------------------------------------------------------------

func TestMatchResultSuppressesScoreMilestone(t *testing.T) {
	n, sent := newTestNotifier()
	prev := base()
	curr := base()
	curr.State = "Complete"
	curr.Status = "India won by 45 runs"
	curr.Runs = 201 // would cross the 200 milestone if not for the early return
	n.checkAndNotify(prev, curr)
	wantOne(t, *sent, "🏆 Match result: India won by 45 runs")
}

func TestMatchResultFallbackStatus(t *testing.T) {
	n, sent := newTestNotifier()
	prev := base()
	curr := base()
	curr.State = "Complete"
	curr.Status = ""
	n.checkAndNotify(prev, curr)
	wantOne(t, *sent, "🏆 Match result: match ended")
}

func TestInningsChangeToZeroInningsIDFiresNothing(t *testing.T) {
	n, sent := newTestNotifier()
	prev := base()
	curr := base()
	curr.InningsID = 0
	n.checkAndNotify(prev, curr)
	if len(*sent) != 0 {
		t.Fatalf("expected no messages when curr.InningsID is 0, got %v", *sent)
	}
}

func TestAlreadyTerminalPrevFiresNothing(t *testing.T) {
	n, sent := newTestNotifier()
	curr := base()
	curr.State = "Complete"
	curr.Status = "India won by 45 runs"
	n.checkAndNotify(curr, curr) // prev == curr, both already terminal
	if len(*sent) != 0 {
		t.Fatalf("expected no messages when prev is already terminal and nothing changed, got %v", *sent)
	}
}

// --- Batter milestones -----------------------------------------------------------

func TestStrikeRotationNoDoubleAnnounce(t *testing.T) {
	n, sent := newTestNotifier()
	prev := base() // Striker Rohit Sharma 48, NonStriker Virat Kohli 34
	curr := base()
	// Strike rotates: Kohli now on strike, Rohit now non-striker; neither crosses a milestone.
	curr.Striker = Batter{Name: "Virat Kohli", Runs: 36, Balls: 42}
	curr.NonStriker = Batter{Name: "Rohit Sharma", Runs: 49, Balls: 36}
	n.checkAndNotify(prev, curr)
	if len(*sent) != 0 {
		t.Fatalf("expected silence across a strike rotation with no milestone crossed, got %v", *sent)
	}
}

func TestBatterMilestoneSkipsIntermediate(t *testing.T) {
	n, sent := newTestNotifier()
	prev := base()
	prev.Striker = Batter{Name: "Rohit Sharma", Runs: 45, Balls: 30}
	curr := base()
	curr.Striker = Batter{Name: "Rohit Sharma", Runs: 105, Balls: 90}
	curr.Runs = 149 // stay in the same 50-bucket as prev (148) so no team milestone interferes
	n.checkAndNotify(prev, curr)
	wantOne(t, *sent, "🎉 Rohit Sharma reaches 100! (105 off 90)")
}

func TestBatterAlreadyPastMilestoneStaysQuiet(t *testing.T) {
	n, sent := newTestNotifier()
	prev := base()
	prev.Striker = Batter{Name: "Rohit Sharma", Runs: 60, Balls: 50}
	curr := base()
	curr.Striker = Batter{Name: "Rohit Sharma", Runs: 66, Balls: 55}
	n.checkAndNotify(prev, curr)
	if len(*sent) != 0 {
		t.Fatalf("expected silence when the batter was already past 50 in prev, got %v", *sent)
	}
}

func TestUnknownBatterNoBaselineStaysQuiet(t *testing.T) {
	n, sent := newTestNotifier()
	prev := base() // Striker Rohit Sharma, NonStriker Virat Kohli -- no "New Batter" here
	curr := base()
	curr.Striker = Batter{Name: "New Batter", Runs: 60, Balls: 45} // retired-hurt returning, or new to the crease
	n.checkAndNotify(prev, curr)
	if len(*sent) != 0 {
		t.Fatalf("expected silence for a batter with no baseline, got %v", *sent)
	}
}

func TestBatterMilestones150And200(t *testing.T) {
	tests := []struct {
		name      string
		prevRuns  int
		currRuns  int
		milestone int
	}{
		{"150", 148, 150, 150},
		{"200", 190, 200, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, sent := newTestNotifier()
			prev := base()
			prev.Striker = Batter{Name: "Rohit Sharma", Runs: tt.prevRuns, Balls: 100}
			curr := base()
			curr.Striker = Batter{Name: "Rohit Sharma", Runs: tt.currRuns, Balls: 110}
			curr.Runs = prev.Runs // keep the team score in the same 50-bucket
			n.checkAndNotify(prev, curr)
			wantOne(t, *sent, fmt.Sprintf("reaches %d!", tt.milestone))
		})
	}
}

func TestRunsForBatterDirect(t *testing.T) {
	s := base() // Striker Rohit Sharma 48, NonStriker Virat Kohli 34
	if runs, ok := s.runsForBatter("Rohit Sharma"); !ok || runs != 48 {
		t.Fatalf("expected striker hit (48, true), got (%d, %v)", runs, ok)
	}
	if runs, ok := s.runsForBatter("Virat Kohli"); !ok || runs != 34 {
		t.Fatalf("expected non-striker hit (34, true), got (%d, %v)", runs, ok)
	}
	if runs, ok := s.runsForBatter("Someone Else"); ok || runs != 0 {
		t.Fatalf("expected an unknown name to be (0, false), got (%d, %v)", runs, ok)
	}
	empty := ScoreState{} // zero-value batters both have Name == ""
	if runs, ok := empty.runsForBatter(""); ok || runs != 0 {
		t.Fatalf("expected an empty name to never match an empty-named batter, got (%d, %v)", runs, ok)
	}
}

func TestEmptyBatterNameSkipped(t *testing.T) {
	n, sent := newTestNotifier()
	prev := base()
	curr := base()
	curr.Striker = Batter{Name: "", Runs: 60, Balls: 40} // no name: must not be treated as a milestone
	n.checkAndNotify(prev, curr)
	if len(*sent) != 0 {
		t.Fatalf("expected an empty batter name to be skipped entirely, got %v", *sent)
	}
}

// --- formatWicket ----------------------------------------------------------------

func TestFormatWicketRealCaptureString(t *testing.T) {
	// Verbatim from the WI vs PAK Day 3 capture: three spaces before "b", two
	// after "10(8)". Irregular whitespace is normal in this field.
	curr := base()
	curr.Team1Name, curr.Team1Short = "West Indies", "WI"
	curr.Team2Name, curr.Team2Short = "Pakistan", "PAK"
	curr.BatTeamShort = "WI"
	curr.Runs, curr.Wickets = 43, 4
	curr.LastWkt = "Shai Hope   b Khurram Shahzad 10(8)  - 43/4 in 16.2 ov."
	got := formatWicket(curr)
	want := "💥 Wicket! Shai Hope out for 10. West Indies 43/4"
	if got != want {
		t.Fatalf("formatWicket() = %q, want %q", got, want)
	}
}

func TestFormatWicketCaught(t *testing.T) {
	curr := base()
	curr.LastWkt = "Kohli c Smith b Starc 34(40) - 187/3 in 30.1 ov."
	got := formatWicket(curr)
	if !strings.Contains(got, "Kohli out for 34") {
		t.Fatalf("expected %q to contain %q", got, "Kohli out for 34")
	}
}

func TestFormatWicketBowled(t *testing.T) {
	curr := base()
	curr.LastWkt = "Rohit b Cummins 12(19) - 45/1 in 10.0 ov."
	got := formatWicket(curr)
	if !strings.Contains(got, "Rohit out for 12") {
		t.Fatalf("expected %q to contain %q", got, "Rohit out for 12")
	}
}

func TestFormatWicketDismissalTypes(t *testing.T) {
	tests := []struct {
		name, lastWkt, wantName, wantRuns string
	}{
		{"lbw", "Babar lbw Bumrah 23(30) - 80/2 in 20.0 ov.", "Babar", "23"},
		{"run out", "Smith run out 15(20) - 90/3 in 22.0 ov.", "Smith", "15"},
		{"stumped", "Rizwan st Pant b Ashwin 40(50) - 120/4 in 28.0 ov.", "Rizwan", "40"},
		{"hit wicket", "Root hit wicket Bumrah 5(8) - 100/5 in 25.0 ov.", "Root", "5"},
		{"retired", "Warner retired hurt 60(70) - 150/2 in 30.0 ov.", "Warner", "60"},
		{"caught and bowled", "Stokes c&b Jadeja 22(30) - 160/3 in 33.0 ov.", "Stokes", "22"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			curr := base()
			curr.LastWkt = tt.lastWkt
			got := formatWicket(curr)
			want := tt.wantName + " out for " + tt.wantRuns
			if !strings.Contains(got, want) {
				t.Fatalf("formatWicket(%q) = %q, expected to contain %q", tt.lastWkt, got, want)
			}
		})
	}
}

func TestFormatWicketNameOnlyNoRuns(t *testing.T) {
	curr := base()
	curr.LastWkt = "Kohli b Starc" // no runs/balls in this shape
	got := formatWicket(curr)
	if !strings.Contains(got, "Kohli out.") {
		t.Fatalf("expected the name-only fallback form, got %q", got)
	}
}

func TestFormatWicketFallbackBare(t *testing.T) {
	for _, lw := range []string{"", "some unparseable garbage string"} {
		curr := base()
		curr.LastWkt = lw
		got := formatWicket(curr)
		want := fmt.Sprintf("💥 Wicket! %s %d/%d", curr.BatTeamName(), curr.Runs, curr.Wickets)
		if got != want {
			t.Fatalf("formatWicket(%q) = %q, want %q", lw, got, want)
		}
	}
}

func TestFormatWicketInitialsName(t *testing.T) {
	curr := base()
	curr.LastWkt = "KL Rahul c Pant b Bumrah 8(11) - 20/1 in 5.0 ov."
	got := formatWicket(curr)
	if !strings.Contains(got, "KL Rahul out for 8") {
		t.Fatalf("expected %q to contain %q", got, "KL Rahul out for 8")
	}
}

// --- Remaining helpers -------------------------------------------------------------

func TestOrdinal(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{1, "1st"}, {2, "2nd"}, {3, "3rd"}, {4, "4th"},
	}
	for _, tt := range tests {
		if got := ordinal(tt.n); got != tt.want {
			t.Fatalf("ordinal(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestCrossedMultiple(t *testing.T) {
	tests := []struct {
		name          string
		prev, curr    int
		wantMilestone int
		wantOK        bool
	}{
		{"no crossing", 110, 120, 0, false},
		{"exact boundary", 149, 150, 150, true},
		{"jump over two multiples", 148, 251, 250, true},
		{"decrease returns false", 150, 100, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := crossedMultiple(tt.prev, tt.curr, 50)
			if ok != tt.wantOK || (ok && m != tt.wantMilestone) {
				t.Fatalf("crossedMultiple(%d, %d, 50) = (%d, %v), want (%d, %v)", tt.prev, tt.curr, m, ok, tt.wantMilestone, tt.wantOK)
			}
		})
	}
}

func TestFormatOvers(t *testing.T) {
	tests := []struct {
		overs float64
		want  string
	}{
		{30.1, "30.1"},
		{0.0, "0.0"},
		{49.6, "49.6"},
		{38.0, "38.0"}, // a whole number renders as "38.0", not "38"
	}
	for _, tt := range tests {
		if got := formatOvers(tt.overs); got != tt.want {
			t.Fatalf("formatOvers(%v) = %q, want %q", tt.overs, got, tt.want)
		}
	}
}

func TestNotifyErrorSwallowedFollowingMessagesStillSent(t *testing.T) {
	var calls int
	var sent []string
	n := &Notifier{send: func(text string) error {
		calls++
		if calls == 1 {
			return errors.New("boom")
		}
		sent = append(sent, text)
		return nil
	}}
	n.notify("first")  // fails; must not block the next call
	n.notify("second") // still attempted, and succeeds
	if len(sent) != 1 || sent[0] != "second" {
		t.Fatalf("expected the second message to still go out despite the first failing, got %v", sent)
	}
}
