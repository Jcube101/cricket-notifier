package main

// notifier.go — the "what changed?" brain.
//
// checkAndNotify takes the previous and current ScoreState and fires a Telegram
// message for each of the six events the project cares about:
//
//   1. Match start          (pre-match -> playing)
//   2. Innings change       (innings id changes)
//   3. Wicket falls         (wicket count goes up)
//   4. Team score milestone (batting team crosses each 50)
//   5. Batting milestone     (a batter crosses 50/100/150/200)
//   6. Match result / end    (-> Complete / Abandoned)
//
// The watch loop is responsible for "seeding": the very first snapshot of a
// match is stored as prev WITHOUT calling this function, so we never replay
// events that happened before the notifier started watching.

import (
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
)

// batterMilestones are the personal scores worth announcing.
var batterMilestones = []int{50, 100, 150, 200}

// Notifier wraps the act of sending a message. Holding a send func (rather than
// calling sendTelegram directly) keeps checkAndNotify easy to unit-test with a
// fake sender.
type Notifier struct {
	send func(text string) error
}

// NewNotifier returns a Notifier bound to a Telegram bot token and chat.
func NewNotifier(token, chatID string) *Notifier {
	return &Notifier{
		send: func(text string) error { return sendTelegram(token, chatID, text) },
	}
}

// checkAndNotify diffs two snapshots and sends a message per detected event.
func (n *Notifier) checkAndNotify(prev, curr ScoreState) {
	// 1. Match start: we were waiting, now we're playing.
	if isPreMatch(prev.State) && !isPreMatch(curr.State) && !isTerminal(curr.State) {
		n.notify(fmt.Sprintf("🏏 %s — Match started", curr.Title()))
	}

	// 6. Match result: handled early so a match that finished between polls is
	// reported even though score fields may have stopped changing.
	if !isTerminal(prev.State) && isTerminal(curr.State) {
		status := strings.TrimSpace(curr.Status)
		if status == "" {
			status = "match ended"
		}
		n.notify(fmt.Sprintf("🏆 Match result: %s", status))
		return // nothing else meaningful to diff once the match is over
	}

	// 2. Innings change: report the innings that just finished, then stop — the
	// score has reset, so score/wicket/batter diffs below would be bogus.
	if prev.InningsID != 0 && curr.InningsID != 0 && curr.InningsID != prev.InningsID {
		n.notify(fmt.Sprintf("🔄 End of %s innings. %s %d/%d",
			ordinal(prev.InningsID), prev.BatTeamName(), prev.Runs, prev.Wickets))
		return
	}

	// The remaining events only make sense within a single, continuous innings.
	if prev.InningsID == 0 || prev.InningsID != curr.InningsID {
		return
	}

	// 3. Wicket falls.
	if curr.Wickets > prev.Wickets {
		n.notify(formatWicket(curr))
	}

	// 4. Team score milestone — crossing each multiple of 50.
	if milestone, ok := crossedMultiple(prev.Runs, curr.Runs, 50); ok {
		n.notify(fmt.Sprintf("📊 %s reach %d (%d/%d in %s overs)",
			curr.BatTeamName(), milestone, curr.Runs, curr.Wickets, formatOvers(curr.Overs)))
	}

	// 5. Batting milestone — for each batter currently at the crease.
	for _, b := range []Batter{curr.Striker, curr.NonStriker} {
		if b.Name == "" {
			continue
		}
		prevRuns := prev.runsForBatter(b.Name) // 0 if this batter is new
		if m, ok := highestMilestone(prevRuns, b.Runs); ok {
			n.notify(fmt.Sprintf("🎉 %s reaches %d! (%d off %d)", b.Name, m, b.Runs, b.Balls))
		}
	}
}

// notify sends one message and logs (but does not fail on) send errors, so one
// failed message never blocks the others.
func (n *Notifier) notify(text string) {
	if err := n.send(text); err != nil {
		slog.Error("failed to send notification", "text", text, "err", err)
		return
	}
	slog.Info("notification sent", "text", text)
}

// runsForBatter finds a batter's runs in this snapshot by name (strike rotates,
// so a player can move between striker and non-striker between polls).
func (s ScoreState) runsForBatter(name string) int {
	switch name {
	case s.Striker.Name:
		return s.Striker.Runs
	case s.NonStriker.Name:
		return s.NonStriker.Runs
	default:
		return 0
	}
}

// --- small pure helpers -----------------------------------------------------

func isPreMatch(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", "preview", "upcoming", "scheduled", "toss":
		return true
	}
	return false
}

func isTerminal(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "complete", "abandoned", "no result":
		return true
	}
	return false
}

// crossedMultiple reports the highest multiple of step that curr has reached
// but prev had not. e.g. prev=148, curr=201, step=50 -> (200, true).
func crossedMultiple(prev, curr, step int) (int, bool) {
	if curr/step > prev/step {
		return (curr / step) * step, true
	}
	return 0, false
}

// highestMilestone returns the largest batter milestone strictly above prevRuns
// and at or below currRuns (so a 45->105 jump reports 100, not 50).
func highestMilestone(prevRuns, currRuns int) (int, bool) {
	best, ok := 0, false
	for _, m := range batterMilestones {
		if prevRuns < m && currRuns >= m {
			best, ok = m, true
		}
	}
	return best, ok
}

// ordinal turns 1,2,3,4 into 1st,2nd,3rd,4th (innings counts only go this high).
func ordinal(n int) string {
	switch n {
	case 1:
		return "1st"
	case 2:
		return "2nd"
	case 3:
		return "3rd"
	default:
		return fmt.Sprintf("%dth", n)
	}
}

// formatOvers prints cricket over notation (e.g. 38.2) without trailing noise.
func formatOvers(o float64) string {
	return strconv.FormatFloat(o, 'f', 1, 64)
}

// wktNamePattern grabs the dismissed batter's name and runs from the lastwkt
// string, e.g. "Kohli c Smith b Starc 34(40) - 187/3 in 30.1 ov." -> Kohli, 34.
var wktNamePattern = regexp.MustCompile(`^(.+?)\s+(?:c |c& ?b|b |lbw|run out|st |hit wicket|retired|not out)`)
var wktRunsPattern = regexp.MustCompile(`(\d+)\(\d+\)`)

// formatWicket builds the wicket message, naming the batter when we can parse
// the lastwkt string and falling back to a plain announcement otherwise.
func formatWicket(curr ScoreState) string {
	score := fmt.Sprintf("%s %d/%d", curr.BatTeamName(), curr.Runs, curr.Wickets)

	name := ""
	if m := wktNamePattern.FindStringSubmatch(curr.LastWkt); m != nil {
		name = strings.TrimSpace(m[1])
	}
	runs := ""
	if m := wktRunsPattern.FindStringSubmatch(curr.LastWkt); m != nil {
		runs = m[1]
	}

	if name != "" && runs != "" {
		return fmt.Sprintf("💥 Wicket! %s out for %s. %s", name, runs, score)
	}
	if name != "" {
		return fmt.Sprintf("💥 Wicket! %s out. %s", name, score)
	}
	return fmt.Sprintf("💥 Wicket! %s", score)
}
