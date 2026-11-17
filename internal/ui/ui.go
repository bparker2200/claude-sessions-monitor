// Package ui renders the terminal views (live, history, usage) and reads raw
// keyboard input. It draws with plain ANSI escapes — alt screen, cursor home,
// erase-to-end-of-line per row — so frames overwrite in place without a clear.
// Every string that originates in a log or the filesystem goes through
// sanitizeForTerminal before it is printed, and widths are counted in runes.
//
// View selection lives in main.go; this package only exposes the renderers.
// See docs/ARCHITECTURE.md for the helpers new views are expected to reuse.
package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yepzdk/claude-sessions-monitor/internal/session"
)

// ANSI color codes
const (
	Reset   = "\033[0m"
	Bold    = "\033[1m"
	Dim     = "\033[2m"
	Green   = "\033[32m"
	Yellow  = "\033[33m"
	Blue    = "\033[34m"
	Red     = "\033[31m"
	Gray    = "\033[90m"
	BgGreen = "\033[42m"
)

// Status symbols (all narrow/single-column width for consistent alignment)
const (
	SymbolWorking    = "●"
	SymbolNeedsInput = "▲"
	SymbolWaiting    = "◉"
	SymbolInactive   = "◌"
)

// flagColor maps a flag severity onto the palette the rest of the dashboard
// already uses for trouble: red is "look at this now", yellow is "look at
// this". An unrecognised severity gets the quieter of the two rather than the
// louder one, so a future tier cannot shout by accident.
func flagColor(worst session.Severity) string {
	if worst == session.SeverityCritical || worst == session.SeverityHigh {
		return Red
	}
	return Yellow
}

// RenderList renders sessions as a simple list (for -l flag)
func RenderList(sessions []session.Session) {
	if len(sessions) == 0 {
		fmt.Println("No active sessions found.")
		return
	}

	showHarness := session.MixedHarnesses(sessions)
	l := calcSessionLayout(getTerminalWidth())

	var buf strings.Builder
	buf.WriteString(sessionHeader(l, "") + "\n")
	buf.WriteString(strings.Repeat("─", l.totalWidth) + "\n")

	for _, s := range sessions {
		renderSessionRow(&buf, s, l, "\n", "", showHarness)
	}

	fmt.Print(buf.String())
}

// FilterByHarness returns only the sessions belonging to h, or all of them when
// h is empty. It is a display filter, not a discovery one: both agents are
// always scanned, so toggling it costs nothing and never hides a session csm
// failed to find.
func FilterByHarness(sessions []session.Session, h session.Harness) []session.Session {
	if h == "" {
		return sessions
	}
	filtered := make([]session.Session, 0, len(sessions))
	for _, s := range sessions {
		if s.Harness == h {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

// sessionHeader returns the column header row matching the given layout.
// gutter is the same left-edge padding the rows use, so the columns line up:
// unselectedMarker in the live view, "" for one-shot output that has no
// selection to show.
func sessionHeader(l sessionLayout, gutter string) string {
	// The gutter is carved out of the session column, the same as in the rows.
	sessionWidth := l.session - len([]rune(gutter))
	if sessionWidth < 1 {
		sessionWidth = 1
	}
	cols := []string{fmt.Sprintf("%-*s", sessionWidth, "SESSION")}
	if l.origin > 0 {
		cols = append(cols, fmt.Sprintf("%-*s", l.origin, "ORIGIN"))
	}
	cols = append(cols,
		fmt.Sprintf("%-*s", l.project, "PROJECT"),
		fmt.Sprintf("%-*s", l.context, "CONTEXT"),
		fmt.Sprintf("%-*s", l.status, "STATUS"),
		fmt.Sprintf("%-*s", l.idle, "IDLE FOR"))
	return gutter + strings.Join(cols, " ")
}

// RenderJSON renders sessions as JSON
func RenderJSON(sessions []session.Session) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(sessions)
}

// rawNewline is the line ending used for interactive (raw terminal mode)
// redraws: erase-to-end-of-line, then CRLF. Erasing per line lets a refresh
// overwrite the previous frame in place instead of clearing the whole screen
// up front — a full clear-then-redraw is what causes the visible flash/blink
// some terminals show on every refresh tick.
const rawNewline = "\033[K\r\n"

// Selection marker drawn in the left gutter of the highlighted row. The gutter
// is the same width whether or not a row is selected, so moving the cursor
// never reflows the table.
const (
	selectedMarker   = "\u258c "
	unselectedMarker = "  "
)

// ActiveSessions returns the sessions the live view shows: everything that
// isn't finished. Callers that need to address a row by index must use this,
// so the selection and the rendered table can't disagree.
//
// Orphaned sessions stay in the list. They are the ones a user most needs to
// act on, and they carry a [ghost] badge that nothing could display while they
// were filtered out here.
func ActiveSessions(sessions []session.Session) []session.Session {
	var active []session.Session
	for _, s := range sessions {
		if s.Status == session.StatusInactive {
			continue
		}
		active = append(active, s)
	}
	return active
}

// LiveView is one frame of the live dashboard. It is a struct because the frame
// outgrew a readable argument list once the harness filter joined it, and every
// field is something the footer or a row has to know.
type LiveView struct {
	// Sessions is the already-filtered set to display.
	Sessions     []session.Session
	WebURL       string // when non-empty, the footer offers the web dashboard
	ClaudeStatus *session.ClaudeStatus
	// Selected indexes ActiveSessions(Sessions), or -1 for no selection.
	Selected int
	// ActionMsg is one line of feedback from the last key the user pressed,
	// or "" for none.
	ActionMsg string
	// UpdateNotice announces a newer csm release, or "" for none.
	UpdateNotice string
	// Filter is the harness the view is restricted to, or "" for all.
	Filter session.Harness
	// Mixed reports whether both agents were present *before* filtering, which
	// is what decides whether rows carry a harness tag. Taken before the filter
	// so narrowing to one agent still shows which one you are looking at.
	Mixed bool
}

// RenderLive renders the live dashboard view.
// Uses \r\n for newlines to work correctly in raw terminal mode.
func RenderLive(v LiveView) {
	// Set terminal title with status summary
	SetTerminalTitle(buildTerminalTitle(v.Sessions))

	// Build the whole frame in memory and write it out in a single syscall.
	// Printing each line separately means the terminal can render partway
	// through a frame — any row whose text changed briefly shows blank (or
	// the old content) between writes, which is what "flicker on updated
	// rows" actually is. One write makes each redraw atomic from the
	// terminal's point of view.
	var buf strings.Builder

	// Move cursor to top without erasing — see rawNewline for why the screen
	// isn't cleared up front.
	buf.WriteString("\033[H")

	// Header
	fmt.Fprintf(&buf, "%sCoding Sessions%s", Bold, Reset)
	if v.Filter != "" {
		fmt.Fprintf(&buf, " %s(%s only)%s", Dim, harnessLabel(v.Filter), Reset)
	}
	fmt.Fprintf(&buf, "%s%s", rawNewline, rawNewline)

	active := ActiveSessions(v.Sessions)

	// Status summary (only active sessions)
	counts := countByStatus(active)
	for i, st := range []session.Status{session.StatusNeedsInput, session.StatusWorking, session.StatusWaiting} {
		if i > 0 {
			buf.WriteString("  ")
		}
		symbol, color := getStatusDisplay(st)
		fmt.Fprintf(&buf, "%s%s %s: %d%s", color, symbol, statusLabel(st), counts[st], Reset)
	}
	buf.WriteString(rawNewline)

	buf.WriteString(rawNewline)

	if len(active) == 0 {
		fmt.Fprintf(&buf, "%s%s%s%s", Dim, emptyLiveMessage(v.Filter), Reset, rawNewline)
	} else {
		l := calcSessionLayout(getTerminalWidth())

		// Column headers
		fmt.Fprintf(&buf, "%s%s", sessionHeader(l, unselectedMarker), rawNewline)
		fmt.Fprintf(&buf, "%s%s", strings.Repeat("─", l.totalWidth), rawNewline)

		for i, s := range active {
			marker := unselectedMarker
			if i == v.Selected {
				marker = selectedMarker
			}
			renderSessionRow(&buf, s, l, rawNewline, marker, v.Mixed)
		}
	}

	// Show Claude service status
	statusLink := terminalLink("https://status.claude.com/", "status.claude.com")
	buf.WriteString(rawNewline)
	if v.ClaudeStatus != nil && v.ClaudeStatus.Available {
		switch v.ClaudeStatus.Indicator {
		case "minor":
			fmt.Fprintf(&buf, "%s%s Claude: %s - %s%s%s", Yellow, "\u26A0", v.ClaudeStatus.Description, statusLink, Reset, rawNewline)
		case "major", "critical":
			fmt.Fprintf(&buf, "%s%s Claude: %s - %s%s%s", Red, "\u2716", v.ClaudeStatus.Description, statusLink, Reset, rawNewline)
		default:
			fmt.Fprintf(&buf, "%sClaude: %s - %s%s%s", Dim, v.ClaudeStatus.Description, statusLink, Reset, rawNewline)
		}
	} else {
		fmt.Fprintf(&buf, "%sClaude: Status unavailable - %s%s%s", Dim, statusLink, Reset, rawNewline)
	}

	// Feedback from the last jump attempt, on its own line so it never shifts
	// the table.
	if v.ActionMsg != "" {
		fmt.Fprintf(&buf, "%s%s%s%s", Dim, sanitizeForTerminal(v.ActionMsg), Reset, rawNewline)
	}

	// A newer release, if one was found. Above the help line so it reads as
	// part of the footer rather than as a row of the table.
	if v.UpdateNotice != "" {
		fmt.Fprintf(&buf, "%s%s%s%s", Yellow, sanitizeForTerminal(v.UpdateNotice), Reset, rawNewline)
	}

	keys := liveHelpKeys(v.Mixed, v.Filter)
	if v.WebURL != "" {
		fmt.Fprintf(&buf, "%s%s | w: open webview (%s) | Ctrl+C: quit%s%s", Dim, keys, v.WebURL, Reset, rawNewline)
	} else {
		fmt.Fprintf(&buf, "%s%s | Ctrl+C: quit%s%s", Dim, keys, Reset, rawNewline)
	}

	fmt.Print(buf.String())
}

// emptyLiveMessage is what the live view says when it has no rows to draw.
//
// A filter that hid every row has to say so. "No active sessions." is what a
// machine with nothing running says, and reading it while your own session is
// running and merely filtered out is how a display filter gets mistaken for a
// broken scan -- so the message names the filter and the key that clears it.
func emptyLiveMessage(filter session.Harness) string {
	if filter == "" {
		return "No active sessions."
	}
	return fmt.Sprintf("No active %s sessions — press f to show every agent.", harnessLabel(filter))
}

// liveHelpKeys is the footer's list of keys.
//
// The harness filter is offered when there is something to filter -- on a
// single-agent machine the key does nothing and advertising it would be noise --
// and always while a filter is set, even after the other agent's sessions have
// gone quiet. A filter outlives the mixed dashboard that allowed it, and
// dropping the key from the footer while its effect was still on screen left
// the user filtered into a view with no advertised way out.
func liveHelpKeys(mixed bool, filter session.Harness) string {
	keys := "↑↓: select | Enter: jump | h: history | u: usage | !: flags"
	if mixed || filter != "" {
		keys += " | f: filter"
	}
	return keys
}

// newlineFor returns the line ending a render function should use: rawNewline
// in interactive mode (showFooter true), or a plain "\n" for one-shot,
// non-terminal output where erase-to-end-of-line escapes would just be noise.
func newlineFor(showFooter bool) string {
	if showFooter {
		return rawNewline
	}
	return "\n"
}

// MoveCursorHome moves the cursor to the top-left without erasing the
// screen. Called once before a redraw so each refresh overwrites the
// previous frame in place — see rawNewline for why a full clear-then-redraw
// causes a visible flash on some terminals.
func MoveCursorHome() {
	fmt.Print("\033[H")
}

// EraseToEnd erases from the cursor to the end of the screen. Called once
// after a full redraw completes, to clear any rows left over from a
// previous, longer frame (e.g. a session or subagent row that's no longer
// there). This is a per-render-cycle concern, not something each view needs
// to remember to do itself — see main.go's render loop for the call site.
func EraseToEnd() {
	fmt.Print("\033[J")
}

// EnterAltScreen switches to the terminal's alternate screen buffer.
// Besides preserving the user's scrollback, this is the conventional signal
// that an application is a full-screen TUI. Block-based terminals (JetBrains'
// terminal, Warp) ignore bare cursor-control sequences on the main buffer and
// only redraw in place once the alternate buffer is active.
func EnterAltScreen() {
	fmt.Print("\033[?1049h")
}

// ExitAltScreen returns to the normal screen buffer, restoring whatever was
// on screen before EnterAltScreen was called.
func ExitAltScreen() {
	fmt.Print("\033[?1049l")
}

// HideCursor hides the terminal cursor
func HideCursor() {
	fmt.Print("\033[?25l")
}

// ShowCursor shows the terminal cursor
func ShowCursor() {
	fmt.Print("\033[?25h")
}

// SetTerminalTitle sets the terminal tab/window title
// The title is sanitized to prevent terminal escape sequence injection
func SetTerminalTitle(title string) {
	fmt.Printf("\033]0;%s\007", sanitizeForTerminal(title))
}

// sanitizeForTerminal removes control characters that could be used
// for terminal escape sequence injection attacks
func sanitizeForTerminal(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1 // Remove control characters
		}
		return r
	}, s)
}

// terminalLink creates a clickable hyperlink using the OSC 8 escape sequence.
// Supported by most modern terminal emulators (iTerm2, macOS Terminal, GNOME Terminal, etc).
func terminalLink(url, text string) string {
	return fmt.Sprintf("\033]8;;%s\033\\%s\033]8;;\033\\", url, text)
}

// ResetTerminalTitle resets the terminal title to default
func ResetTerminalTitle() {
	fmt.Print("\033]0;\007")
}

// buildTerminalTitle creates a status summary for the terminal title
func buildTerminalTitle(sessions []session.Session) string {
	counts := countByStatus(ActiveSessions(sessions))

	// Priority: Needs you > Working > Idle
	var parts []string

	if n := counts[session.StatusNeedsInput]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d need you", n))
	}
	if n := counts[session.StatusWorking]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d working", n))
	}
	if n := counts[session.StatusWaiting]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d idle", n))
	}

	if len(parts) == 0 {
		return "CSM: no active sessions"
	}

	return "CSM: " + strings.Join(parts, ", ")
}

// getStatusDisplay returns the symbol and color for a status
//
// Red is reserved for Needs you, the one state that wants something from the
// user. Idle is green because a finished turn is the good outcome; Working is
// left uncoloured because there is nothing to do about it yet.
func getStatusDisplay(status session.Status) (string, string) {
	switch status {
	case session.StatusWorking:
		return SymbolWorking, Reset
	case session.StatusNeedsInput:
		return SymbolNeedsInput, Red
	case session.StatusWaiting:
		return SymbolWaiting, Green
	case session.StatusInactive:
		return SymbolInactive, Dim
	default:
		return SymbolInactive, Reset
	}
}

// statusLabel is the word the terminal shows for a status.
//
// The Status values themselves are not renamed: they are the JSON API and the
// web dashboard's vocabulary too, and "Waiting" meaning "finished its turn" is
// what this label exists to stop the terminal from saying. Only the words a
// person reads change here.
func statusLabel(status session.Status) string {
	switch status {
	case session.StatusNeedsInput:
		return "Needs you"
	case session.StatusWaiting:
		return "Idle"
	default:
		return string(status)
	}
}

// formatStatus formats the status cell with symbol and padding to exact width
func formatStatus(status session.Status, width int) string {
	symbol, color := getStatusDisplay(status)
	label := statusLabel(status)
	text := symbol + " " + label
	visibleLen := 2 + utf8.RuneCountInString(label) // symbol(1) + space(1) + label

	// Pad to width
	if visibleLen < width {
		text += strings.Repeat(" ", width-visibleLen)
	}

	return color + text + Reset
}

// countByStatus counts sessions by their status
func countByStatus(sessions []session.Session) map[session.Status]int {
	counts := make(map[session.Status]int)
	for _, s := range sessions {
		counts[s.Status]++
	}
	return counts
}

// formatElapsed formats a duration as a human-readable elapsed time
func formatElapsed(d time.Duration) string {
	if d < time.Second {
		return "just now"
	}
	return session.FormatAge(d) + " ago"
}

// truncate truncates a string to a maximum visible length (in runes, not bytes).
// This ensures multi-byte UTF-8 characters are not split mid-character.
func truncate(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-3]) + "..."
}

// cutRunes cuts s to at most n runes with no ellipsis, for the fixed-width
// cells where a "..." would cost more of the value than it explains. Runes, not
// bytes: slicing by byte can split a multi-byte character in half and
// mis-measures the padding.
func cutRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// sectionRule writes a "━━━ Title ━━━━━" divider that fills the width. The
// trailing run is what is left of width after the leading "━━━ ", the title and
// one space, and never shrinks below one character on a narrow terminal.
func sectionRule(buf *strings.Builder, title string, width int, nl string) {
	fill := width - 5 - utf8.RuneCountInString(title)
	if fill < 1 {
		fill = 1
	}
	fmt.Fprintf(buf, "%s━━━ %s %s%s%s", Dim, title, strings.Repeat("━", fill), Reset, nl)
}

// contextBarWidth is the number of block characters in the progress bar
const contextBarWidth = 10

// formatContext renders a visual progress bar with percentage label
// Example: "████████░░ 80%"
func formatContext(s session.Session, width int) string {
	if s.ContextTokens == 0 {
		text := "-"
		if len(text) < width {
			text += strings.Repeat(" ", width-len(text))
		}
		return Dim + text + Reset
	}

	// Clamp percentage to 0-100
	pct := s.ContextPercent
	if pct > 100 {
		pct = 100
	}

	// Calculate filled vs empty blocks
	filled := int(pct / 100 * float64(contextBarWidth))
	if filled > contextBarWidth {
		filled = contextBarWidth
	}
	empty := contextBarWidth - filled

	// Color based on percentage
	var color string
	switch {
	case pct >= 91:
		color = Red
	case pct >= 76:
		color = Yellow
	default:
		color = Green
	}

	// Build bar: colored filled blocks + dim empty blocks + percentage
	label := fmt.Sprintf(" %.0f%%", pct)

	// Append a marker when the active model uses an extended context window so
	// users can tell at a glance that "24%" is of 1M, not 200K.
	suffix := ""
	if s.ContextWindow > session.DefaultContextWindow {
		suffix = " (1M)"
	}

	bar := color + strings.Repeat("█", filled) + Reset +
		Dim + strings.Repeat("░", empty) + Reset +
		label
	if suffix != "" {
		bar += Dim + suffix + Reset
	}

	// Pad to width (visible length = bar chars + label chars + suffix chars)
	visibleLen := contextBarWidth + len(label) + len(suffix)
	if visibleLen < width {
		bar += strings.Repeat(" ", width-visibleLen)
	}

	return bar
}

// blank is what a cell shows when the fact it would hold is not on disk.
const blank = "—"

// padCell pads already-coloured text to width visible runes, given how many
// visible runes it holds.
func padCell(cell string, visible, width int) string {
	if visible < width {
		cell += strings.Repeat(" ", width-visible)
	}
	return cell
}

// ownDesktopApp maps each agent to the origin slug of its own desktop app.
var ownDesktopApp = map[session.Harness]string{
	session.HarnessClaude: "claude-desktop",
	session.HarnessCodex:  "codex-desktop",
}

// originWhere is the "where" half of the origin cell: the app the session runs
// in. The agent's *own* desktop app is shortened to "Desktop", because the
// agent half already names it ("Claude · Desktop", not "Claude · Claude
// Desktop"). Another agent's desktop app keeps its name: a Codex session in
// Claude Desktop's terminal is not the Codex desktop app, and shortening both
// to "Desktop" made the two read the same.
func originWhere(s session.Session) string {
	if app, ok := ownDesktopApp[s.Harness]; ok && s.Origin.App == app {
		return "Desktop"
	}
	return s.Origin.Display
}

// formatOrigin renders the origin cell as "Agent · Where", padded to exactly
// width visible chars, or "" when the column is disabled (width == 0).
//
// The agent is always named, not only when several agents are running: which
// agent a session belongs to and what launched it are one fact about where it
// came from. An origin csm could not detect leaves the agent on its own.
func formatOrigin(s session.Session, width int) string {
	if width <= 0 {
		return ""
	}

	agent := agentName(s.Harness)
	where := sanitizeForTerminal(originWhere(s))

	var color string
	switch s.Origin.Category {
	case session.OriginTerminal:
		color = Gray
	case session.OriginIDE:
		color = Blue
	case session.OriginDesktop:
		color = Yellow
	default:
		color = Dim
	}

	switch {
	case agent == "" && where == "":
		return padCell(Dim+blank+Reset, 1, width)
	case where == "":
		agent = cutRunes(agent, width)
		return padCell(agent, utf8.RuneCountInString(agent), width)
	case agent == "":
		where = cutRunes(where, width)
		return padCell(color+where+Reset, utf8.RuneCountInString(where), width)
	}

	const sep = " · "
	agent = cutRunes(agent, width)
	room := width - utf8.RuneCountInString(agent) - utf8.RuneCountInString(sep)
	if room < 1 {
		return padCell(agent, utf8.RuneCountInString(agent), width)
	}
	where = cutRunes(where, room)
	cell := agent + Dim + sep + Reset + color + where + Reset
	visible := utf8.RuneCountInString(agent) + utf8.RuneCountInString(sep) + utf8.RuneCountInString(where)
	return padCell(cell, visible, width)
}

// shortID is the part of a session id the session cell shows: enough to tell
// sessions apart and to find the log, without the full 36 characters.
func shortID(id string) string {
	return cutRunes(id, 8)
}

// formatSession renders the session cell: the session's name, then its short
// id in parentheses, padded to width. A session with no name shows the blank
// so the id still has something to qualify; the id is kept whole and the name
// is what gets cut when space runs short.
func formatSession(s session.Session, width int) string {
	if width <= 0 {
		return ""
	}
	name := sanitizeForTerminal(s.SessionTitle)
	nameColor := ""
	if name == "" {
		name, nameColor = blank, Dim
	}

	id := ""
	if s.SessionID != "" {
		id = " (" + sanitizeForTerminal(shortID(s.SessionID)) + ")"
	}
	idLen := utf8.RuneCountInString(id)
	if idLen >= width {
		id, idLen = "", 0
	}

	name = truncate(name, width-idLen)
	cell := nameColor + name + Reset
	if id != "" {
		cell += Dim + id + Reset
	}
	return padCell(cell, utf8.RuneCountInString(name)+idLen, width)
}

// idleFor renders how long an Idle session has been sitting since its last
// turn, padded to width. Every other status shows the blank: "idle for" means
// nothing while a session is working or waiting on the user.
func idleFor(s session.Session, width int, now time.Time) string {
	if s.Status != session.StatusWaiting || s.LastActivity.IsZero() {
		return padCell(Dim+blank+Reset, 1, width)
	}
	text := cutRunes(formatIdleDuration(now.Sub(s.LastActivity)), width)
	return padCell(text, utf8.RuneCountInString(text), width)
}

// formatIdleDuration is a compact two-unit duration: "45s", "42m", "3h 10m",
// "2d 4h". Two units because "3h" hides whether the session has been idle for
// three hours or nearly four.
func formatIdleDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		if d < 0 {
			d = 0
		}
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// lastTurnPrefix labels the second line of a session block: the text of the
// session's last reply.
const lastTurnPrefix = "last turn › "

// renderSessionRow renders a single session row using the given layout.
// The main row shows session name and id, origin (optional), project, context,
// status and idle time. A second indented line shows the session's last reply,
// followed by any subagent rows. marker fills the left gutter; its width is
// carved out of the row, so selected and unselected markers must be the same
// width.
//
// showHarness puts the agent tag in the project cell when the terminal is too
// narrow for the origin column, which otherwise names the agent: on a mixed
// dashboard an untagged row is an ambiguous row, whatever the width.
func renderSessionRow(buf *strings.Builder, s session.Session, l sessionLayout, nl string, marker string,
	showHarness bool) {
	// The gutter is carved out of the session column rather than added to the
	// row: calcSessionLayout already spends the full terminal width, so widening
	// the row here would wrap every line.
	gutter := len([]rune(marker))
	sessionWidth := l.session - gutter
	if sessionWidth < 1 {
		sessionWidth = 1
	}

	cols := []string{formatSession(s, sessionWidth)}
	if l.origin > 0 {
		cols = append(cols, formatOrigin(s, l.origin), formatProject(s, l.project, false))
	} else {
		cols = append(cols, formatProject(s, l.project, showHarness))
	}
	cols = append(cols,
		formatContext(s, l.context),
		formatStatus(s.Status, l.status),
		idleFor(s, l.idle, time.Now()))
	buf.WriteString(marker + strings.Join(cols, " ") + nl)

	// Second line: the session's last reply, falling back to what it is doing
	// when it has not replied yet. Sanitized: it is log content.
	desc := sanitizeForTerminal(s.LastMessage)
	if desc == "" {
		desc = sanitizeForTerminal(s.Task)
	}
	if desc != "" && desc != "-" {
		indent := gutter + 2
		msgWidth := l.totalWidth - indent - utf8.RuneCountInString(lastTurnPrefix)
		if msgWidth > 0 {
			fmt.Fprintf(buf, "%s%s%s%s%s", strings.Repeat(" ", indent),
				Dim, lastTurnPrefix, truncate(desc, msgWidth), Reset+nl)
		}
	}

	// Nested subagent rows, indented under their parent session
	for _, sa := range s.Subagents {
		renderSubagentRow(buf, sa, l, nl, gutter)
	}

	// Blank line after each session block for visual grouping
	buf.WriteString(nl)
}

// Indentation for nested subagent rows: "  └ " before the status symbol, and
// the description line indented past it.
const (
	subagentIndent     = "  └ "
	subagentIndentLen  = 4
	subagentDescIndent = 6
)

// renderSubagentRow renders one subagent as an indented child of its session.
func renderSubagentRow(buf *strings.Builder, sa session.Subagent, l sessionLayout, nl string, gutter int) {
	activity := "Now"
	if elapsed := time.Since(sa.LastActivity); elapsed >= time.Minute {
		activity = formatElapsed(elapsed)
	}

	label := sanitizeForTerminal(sa.Label())
	if sa.Blocking {
		label += " (blocking)"
	}

	// Label column absorbs everything the fixed columns don't use, so the
	// activity lines up under the parent table's last column (IDLE FOR).
	activityWidth := l.idle
	labelWidth := l.totalWidth - gutter - subagentIndentLen - 2 - activityWidth - 1
	if labelWidth < 1 {
		labelWidth = 1
	}
	label = truncate(label, labelWidth)

	fmt.Fprintf(buf, "%s%s%s%s%s %s%-*s%s %-*s%s",
		strings.Repeat(" ", gutter),
		subagentIndent,
		Green, SymbolWorking, Reset,
		Dim, labelWidth, label, Reset,
		activityWidth, activity,
		nl)

	desc := sanitizeForTerminal(sa.Description)
	if task := sanitizeForTerminal(sa.Task); task != "" {
		desc = task
	}
	if desc != "" && desc != "-" {
		indent := gutter + subagentDescIndent
		descWidth := l.totalWidth - indent
		if descWidth > 0 {
			fmt.Fprintf(buf, "%s%s%s%s",
				strings.Repeat(" ", indent),
				Dim, truncate(desc, descWidth), Reset+nl)
		}
	}
}

// harnessLabel is the short name a harness goes by in the UI.
func harnessLabel(h session.Harness) string {
	switch h {
	case session.HarnessClaude:
		return "cc"
	case session.HarnessOMP:
		return "omp"
	case session.HarnessCodex:
		return "codex"
	default:
		return string(h)
	}
}

// agentName is how the origin cell names a harness: in words, because it reads
// as the first half of "Claude · Ghostty" rather than as a badge.
func agentName(h session.Harness) string {
	switch h {
	case session.HarnessClaude:
		return "Claude"
	case session.HarnessCodex:
		return "Codex"
	case session.HarnessOMP:
		return "omp"
	default:
		return string(h)
	}
}

// formatProject formats the project name with optional indicators, padded to maxLen visible chars
func formatProject(s session.Session, maxLen int, showHarness bool) string {
	// Sanitize to prevent ANSI escape injection from log/filesystem content
	name := sanitizeForTerminal(s.Project)
	var suffixes []string
	var suffixLens []int // visible length of each suffix (excluding space)

	// add appends one badge, measuring the width from the text it is given so
	// the two slices cannot fall out of step and no width is restated by hand.
	add := func(color, text string) {
		suffixes = append(suffixes, color+text+Reset)
		suffixLens = append(suffixLens, utf8.RuneCountInString(text))
	}

	// Which agent this row belongs to, first so the width-shedding loop below
	// drops it last: on a mixed dashboard, knowing which agent a row is matters
	// more than its branch or title.
	if showHarness && s.Harness != "" {
		add(Dim, "["+harnessLabel(s.Harness)+"]")
	}

	// Add git branch if present (show first, most useful)
	if s.GitBranch != "" {
		add(Dim, "@"+cutRunes(sanitizeForTerminal(s.GitBranch), 12))
	}

	// The session title is not repeated here: it has the SESSION column.

	// Ghost indicator (highest priority warning)
	if s.IsGhost {
		add(Red, "[ghost]")
	}

	// Incomplete data warning: this row's numbers are partly missing, so they
	// must not read as measurements.
	if s.Degraded != "" {
		add(Yellow, "[?]")
	}

	// Flags: what csm recognised in the log. The count is on the badge because
	// one escalation and fifteen are different situations, and the colour is the
	// worst severity so a row cannot look calm while carrying a critical flag.
	// There is deliberately no badge for zero: csm cannot see a command it does
	// not recognise, so an unbadged row means "nothing recognised", not "safe".
	if n := s.Flags.Total(); n > 0 {
		add(flagColor(s.Flags.Worst), "[!"+strconv.Itoa(n)+"]")
	}

	// Drop suffixes from the end until they fit, keeping at least 4 chars for the name
	const minNameWidth = 4
	totalSuffixLen := 0
	for _, l := range suffixLens {
		totalSuffixLen += 1 + l // space + indicator
	}
	for len(suffixes) > 0 && maxLen-totalSuffixLen < minNameWidth {
		last := len(suffixLens) - 1
		totalSuffixLen -= 1 + suffixLens[last]
		suffixes = suffixes[:last]
		suffixLens = suffixLens[:last]
	}

	// Truncate name to fit
	nameWidth := maxLen - totalSuffixLen
	if nameWidth < 1 {
		nameWidth = 1
	}
	truncated := truncate(name, nameWidth)
	// Runes, not bytes: truncate cut by rune, and padding to a byte count makes
	// every column right of a non-ASCII project name shift left.
	visibleLen := utf8.RuneCountInString(truncated)

	// Build result
	result := truncated
	for i, suffix := range suffixes {
		result += " " + suffix
		visibleLen += 1 + suffixLens[i] // space + indicator visible length
	}

	// Pad to maxLen with spaces (ANSI codes don't count for visual width)
	if visibleLen < maxLen {
		result += strings.Repeat(" ", maxLen-visibleLen)
	}

	return result
}
