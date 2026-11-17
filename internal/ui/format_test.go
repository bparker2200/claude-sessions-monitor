package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/yepzdk/claude-sessions-monitor/internal/session"
)

// stripANSI returns what a terminal actually draws, without the colour codes.
func stripANSI(s string) string {
	var out []rune
	inEscape := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			inEscape = true
		case inEscape && r == 'm':
			inEscape = false
		case !inEscape:
			out = append(out, r)
		}
	}
	return string(out)
}

// visibleWidth counts the runes a terminal actually draws, ignoring ANSI
// colour codes.
func visibleWidth(s string) int {
	return len([]rune(stripANSI(s)))
}

// The project column is padded to a fixed width. Measuring that padding in
// bytes while truncating in runes shifts every column to the right of a
// non-ASCII project name.
func TestFormatProjectPadsToRuneWidth(t *testing.T) {
	const width = 30
	for _, name := range []string{
		"my-api-service",
		"проект-мониторинг",
		"项目-监控",
		"café-app",
		"a",
		strings.Repeat("ü", 60),
	} {
		got := visibleWidth(formatProject(session.Session{Project: name}, width, false))
		if got != width {
			t.Errorf("project %q: visible width = %d, want %d", name, got, width)
		}
	}
}

// Every cell is padded to its column's width whatever it holds, or every
// column to its right shifts.
func TestFormatOriginPadsToRuneWidth(t *testing.T) {
	for _, h := range []session.Harness{session.HarnessClaude, session.HarnessOMP, ""} {
		for _, display := range []string{"Ghostty", "", "iTerm", "GNOME Terminal", "Ünicode", "非常長的名字"} {
			s := session.Session{Origin: session.Origin{Display: display}, Harness: h}
			if got := visibleWidth(formatOrigin(s, fixedOriginWidth)); got != fixedOriginWidth {
				t.Errorf("origin %q (agent %q): visible width = %d, want %d", display, h, got, fixedOriginWidth)
			}
		}
	}
}

// The origin cell names the agent and where it runs, as one phrase. A desktop
// app is shortened so the agent is not named twice, and an undetected origin
// leaves the agent standing alone rather than a dangling separator.
func TestFormatOriginReadsAgentThenWhere(t *testing.T) {
	cell := func(o session.Origin, h session.Harness) string {
		return strings.TrimRight(stripANSI(formatOrigin(session.Session{Origin: o, Harness: h}, fixedOriginWidth)), " ")
	}
	for _, tc := range []struct {
		origin session.Origin
		h      session.Harness
		want   string
	}{
		{session.Origin{Display: "Ghostty", Category: session.OriginTerminal}, session.HarnessClaude, "Claude · Ghostty"},
		{session.Origin{App: "claude-desktop", Display: "Claude Desktop", Category: session.OriginDesktop}, session.HarnessClaude, "Claude · Desktop"},
		{session.Origin{App: "codex-desktop", Display: "Codex Desktop", Category: session.OriginDesktop}, session.HarnessCodex, "Codex · Desktop"},
		// A Codex session in Claude Desktop's terminal is not the Codex desktop app.
		{session.Origin{App: "claude-desktop", Display: "Claude Desktop", Category: session.OriginDesktop}, session.HarnessCodex, "Codex · Claude Des"},
		{session.Origin{}, session.HarnessClaude, "Claude"},
		{session.Origin{}, "", "—"},
		{session.Origin{Display: "GNOME Terminal", Category: session.OriginTerminal}, session.HarnessClaude, "Claude · GNOME Ter"},
	} {
		if got := cell(tc.origin, tc.h); got != tc.want {
			t.Errorf("origin %+v, agent %q: cell = %q, want %q", tc.origin, tc.h, got, tc.want)
		}
	}
}

// The session cell is the name, then the short id. A long name is what gets
// cut, never the id, which is how the session is found again; a session with
// no name shows the blank, not an empty cell.
func TestFormatSessionKeepsTheIDAndCutsTheName(t *testing.T) {
	const width = 30
	id := "5e6f7a8b-1111-2222-3333-444455556666"

	got := stripANSI(formatSession(session.Session{SessionTitle: "Weekly review", SessionID: id}, width))
	if !strings.HasPrefix(got, "Weekly review (5e6f7a8b)") || visibleWidth(got) != width {
		t.Errorf("named session cell = %q (width %d), want %q padded to %d",
			got, visibleWidth(got), "Weekly review (5e6f7a8b)", width)
	}

	long := stripANSI(formatSession(session.Session{SessionTitle: strings.Repeat("Long matter name ", 4), SessionID: id}, width))
	if !strings.HasSuffix(strings.TrimRight(long, " "), "(5e6f7a8b)") || visibleWidth(long) != width {
		t.Errorf("long name cell = %q; the id was cut or the cell is not %d wide", long, width)
	}

	unnamed := stripANSI(formatSession(session.Session{SessionID: id}, width))
	if !strings.HasPrefix(unnamed, "— (5e6f7a8b)") {
		t.Errorf("unnamed session cell = %q, want the blank before the id", unnamed)
	}
}

// The terminal says Needs you / Working / Idle; Needs you alone is red. The
// Status values underneath are the API's and must not change.
func TestStatusWordsAndColours(t *testing.T) {
	for _, tc := range []struct {
		status session.Status
		word   string
		color  string
	}{
		{session.StatusNeedsInput, "Needs you", Red},
		{session.StatusWorking, "Working", Reset},
		{session.StatusWaiting, "Idle", Green},
	} {
		if got := statusLabel(tc.status); got != tc.word {
			t.Errorf("status %q reads %q, want %q", tc.status, got, tc.word)
		}
		if _, color := getStatusDisplay(tc.status); color != tc.color {
			t.Errorf("status %q colour = %q, want %q", tc.status, color, tc.color)
		}
		if got := visibleWidth(formatStatus(tc.status, fixedStatusWidth)); got != fixedStatusWidth {
			t.Errorf("status %q cell width = %d, want %d", tc.status, got, fixedStatusWidth)
		}
	}
	if session.StatusWaiting != "Waiting" || session.StatusNeedsInput != "Needs Input" {
		t.Error("an internal Status value was renamed; the JSON API and the web dashboard depend on them")
	}
}

// "Idle for" counts only on Idle rows; on a working or blocked session it would
// be a number that means nothing.
func TestIdleForShowsOnlyOnIdleRows(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	last := now.Add(-(3*time.Hour + 10*time.Minute))
	cell := func(st session.Status) string {
		return strings.TrimRight(stripANSI(idleFor(session.Session{Status: st, LastActivity: last}, fixedIdleWidth, now)), " ")
	}
	if got := cell(session.StatusWaiting); got != "3h 10m" {
		t.Errorf("idle session shows %q, want %q", got, "3h 10m")
	}
	for _, st := range []session.Status{session.StatusWorking, session.StatusNeedsInput} {
		if got := cell(st); got != "—" {
			t.Errorf("%s session shows idle time %q, want the blank", st, got)
		}
	}
	for d, want := range map[time.Duration]string{
		45 * time.Second:             "45s",
		42 * time.Minute:             "42m",
		50*time.Hour + 5*time.Minute: "2d 2h",
	} {
		if got := formatIdleDuration(d); got != want {
			t.Errorf("formatIdleDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

// A rendered row is exactly the terminal width, in both layouts, and the
// second line is labelled as the session's last turn.
func TestRenderSessionRowFillsTheWidthAndLabelsTheLastTurn(t *testing.T) {
	s := session.Session{
		Project:      "work/api",
		Status:       session.StatusWaiting,
		Harness:      session.HarnessClaude,
		SessionTitle: "Weekly review",
		SessionID:    "5e6f7a8b-1111-2222-3333-444455556666",
		LastMessage:  "Done. Three items moved to next week.",
		LastActivity: time.Now().Add(-42 * time.Minute),
		Origin:       session.Origin{Display: "Ghostty", Category: session.OriginTerminal},
	}
	for _, width := range []int{120, originColumnMinTTY - 1} {
		l := calcSessionLayout(width)
		var buf strings.Builder
		renderSessionRow(&buf, s, l, "\n", "  ", true)
		lines := strings.Split(stripANSI(buf.String()), "\n")
		if got := visibleWidth(lines[0]); got != width {
			t.Errorf("width %d: row is %d wide, so it wraps or leaves a gap:\n%s", width, got, lines[0])
		}
		if !strings.Contains(lines[1], lastTurnPrefix+"Done. Three items") {
			t.Errorf("width %d: second line = %q, want the last reply after %q", width, lines[1], lastTurnPrefix)
		}
	}
}

// Below originColumnMinTTY the origin column is dropped, and with it the cell
// that names the agent. On a mixed dashboard the agent falls back to the
// project cell: an untagged row is an ambiguous row, whatever the width.
func TestRenderSessionRowKeepsHarnessOnNarrowTerminal(t *testing.T) {
	s := session.Session{
		Project: "work/api",
		Status:  session.StatusWorking,
		Harness: session.HarnessOMP,
		Origin:  session.Origin{Display: "Ghostty", Category: session.OriginTerminal},
	}
	for name, width := range map[string]int{"wide": 120, "narrow": originColumnMinTTY - 1} {
		var buf strings.Builder
		renderSessionRow(&buf, s, calcSessionLayout(width), "\n", "  ", true)
		if !strings.Contains(stripANSI(buf.String()), "omp") {
			t.Errorf("%s layout lost the agent label:\n%s", name, stripANSI(buf.String()))
		}
	}
}

// A row whose log could not be fully read must say so, or its numbers read as
// measurements.
func TestFormatProjectMarksDegradedRow(t *testing.T) {
	out := formatProject(session.Session{Project: "api", Degraded: "permission denied"}, 30, false)
	if !strings.Contains(out, "[?]") {
		t.Errorf("degraded session is not marked: %q", out)
	}
	if visibleWidth(out) != 30 {
		t.Errorf("visible width = %d, want 30", visibleWidth(out))
	}
}

// The tag is what tells a mixed dashboard's rows apart, so it must render, must
// not disturb the column width, and must stay away when there is only one agent
// to see -- a single-agent user should get exactly the dashboard they had.
func TestFormatProjectShowsHarnessTagOnlyWhenAsked(t *testing.T) {
	const width = 30
	s := session.Session{Project: "api", Harness: session.HarnessOMP}

	tagged := formatProject(s, width, true)
	if !strings.Contains(tagged, "[omp]") {
		t.Errorf("harness tag missing: %q", tagged)
	}
	if visibleWidth(tagged) != width {
		t.Errorf("tagged cell width = %d, want %d; the columns after it shift",
			visibleWidth(tagged), width)
	}

	plain := formatProject(s, width, false)
	if strings.Contains(plain, "[omp]") {
		t.Errorf("harness tag shown on a single-agent dashboard: %q", plain)
	}
}

// The tag survives a narrow column longer than the branch and title do: on a
// mixed dashboard, which agent a row belongs to matters more than either.
func TestFormatProjectKeepsHarnessTagWhenCrowded(t *testing.T) {
	s := session.Session{
		Project:      "some-long-project-name",
		Harness:      session.HarnessClaude,
		GitBranch:    "feature/very-long-branch",
		SessionTitle: "a session title that will not fit",
	}

	out := formatProject(s, 16, true)
	if !strings.Contains(out, "[cc]") {
		t.Errorf("harness tag was dropped before the branch and title: %q", out)
	}
	if visibleWidth(out) != 16 {
		t.Errorf("visible width = %d, want 16", visibleWidth(out))
	}
}

func TestFilterByHarness(t *testing.T) {
	in := []session.Session{
		{Project: "a", Harness: session.HarnessClaude},
		{Project: "b", Harness: session.HarnessOMP},
		{Project: "c", Harness: session.HarnessClaude},
	}

	if got := FilterByHarness(in, ""); len(got) != 3 {
		t.Errorf("no filter returned %d of 3 sessions", len(got))
	}
	got := FilterByHarness(in, session.HarnessOMP)
	if len(got) != 1 || got[0].Project != "b" {
		t.Errorf("filtered to omp = %+v, want just b", got)
	}
}

// A filter outlives the mixed dashboard that allowed it: filter to the other
// agent, let its sessions go idle, and `Mixed` is false again while the rows are
// still hidden. If the footer stops naming `f` there, nothing on screen says how
// to get the rows back and restarting csm is the only way out.
func TestLiveHelpKeysAlwaysOffersTheKeyOutOfAFilter(t *testing.T) {
	if got := liveHelpKeys(false, session.HarnessClaude); !strings.Contains(got, "f: filter") {
		t.Errorf("footer = %q; a filtered view with no `f` offered cannot be undone", got)
	}
	if got := liveHelpKeys(true, ""); !strings.Contains(got, "f: filter") {
		t.Errorf("footer = %q, want `f` offered on a mixed dashboard", got)
	}
	// A single-agent machine with no filter: the key does nothing, so naming it
	// is noise.
	if got := liveHelpKeys(false, ""); strings.Contains(got, "f: filter") {
		t.Errorf("footer = %q, want no filter key on a single-agent dashboard", got)
	}
}

// "No active sessions." while your own session is running and merely filtered
// out reads as a broken scan. That is the report this fix came from.
func TestEmptyLiveMessageNamesTheFilterThatHidTheRows(t *testing.T) {
	got := emptyLiveMessage(session.HarnessClaude)
	if !strings.Contains(got, "cc") {
		t.Errorf("message = %q, want the filter named", got)
	}
	if !strings.Contains(got, "f") {
		t.Errorf("message = %q, want the key that clears it", got)
	}

	if got := emptyLiveMessage(""); got != "No active sessions." {
		t.Errorf("unfiltered message = %q, want the plain sentence", got)
	}
}

func TestActiveSessionsKeepsGhosts(t *testing.T) {
	in := []session.Session{
		{Project: "orphan", Status: session.StatusWaiting, IsGhost: true},
		{Project: "done", Status: session.StatusInactive},
		{Project: "live", Status: session.StatusWorking},
	}
	got := ActiveSessions(in)
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2 (ghost kept, inactive dropped)", len(got))
	}
	if !got[0].IsGhost {
		t.Error("ghost session was filtered out; its [ghost] badge can never render")
	}
}

// A history row whose log stopped short must say so, or its "0s, 0 msgs" reads
// as a measurement. The marker also has to fit the project column, or every
// column to its right shifts.
func TestHistoryProjectCellMarksUnreadableLog(t *testing.T) {
	const width = 20
	good := historyProjectCell(session.HistorySession{Project: "api"}, width)
	if visibleWidth(good) != width {
		t.Errorf("plain cell width = %d, want %d", visibleWidth(good), width)
	}

	// A short name so the cell has to pad around the marker: padding it by
	// byte count instead measures the marker's colour codes as visible.
	marked := historyProjectCell(session.HistorySession{
		Project:  "api",
		Degraded: "scan stopped early",
	}, width)
	if !strings.Contains(marked, "[?]") {
		t.Errorf("degraded row is not marked: %q", marked)
	}
	if visibleWidth(marked) != width {
		t.Errorf("marked cell width = %d, want %d; the columns after it shift", visibleWidth(marked), width)
	}
}

// The "(1M)" marker says a percentage is of a 1M window, not the 200K default.
// It reads the decision the server already made, because deriving it a second
// time is how the browser once showed 200K for a session the TUI showed as 1M.
func TestContextBarMarksAnExtendedWindow(t *testing.T) {
	extended := session.Session{
		ContextTokens:  240000,
		ContextPercent: 24,
		Model:          "claude-opus-5",
		ContextWindow:  session.ExtendedContextWindow,
	}
	if got := stripANSI(formatContext(extended, 30)); !strings.Contains(got, "(1M)") {
		t.Errorf("context cell %q has no (1M) marker; 24%% of 1M reads as 24%% of 200K", got)
	}

	standard := extended
	standard.ContextWindow = session.DefaultContextWindow
	if got := stripANSI(formatContext(standard, 30)); strings.Contains(got, "(1M)") {
		t.Errorf("context cell %q claims a 1M window for a 200K session", got)
	}
}
