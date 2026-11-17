package ui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Every width a session row uses has to add up to the terminal width, or the
// row wraps and the table falls apart.
func TestCalcSessionLayoutSpendsTheWholeWidth(t *testing.T) {
	for _, width := range []int{80, 99, 100, 120, 140, 200} {
		l := calcSessionLayout(width)
		if l.totalWidth != width {
			t.Errorf("width %d: columns add up to %d (session=%d origin=%d project=%d context=%d status=%d idle=%d); "+
				"the row wraps or leaves a gap", width, l.totalWidth, l.session, l.origin, l.project, l.context, l.status, l.idle)
		}
		if l.context != fixedContextWidth || l.status != fixedStatusWidth || l.idle != fixedIdleWidth {
			t.Errorf("width %d: a fixed column changed width (context=%d status=%d idle=%d)",
				width, l.context, l.status, l.idle)
		}
	}
}

// The session name is the column you read first, so it gets the larger share
// of the spare width, and the project stays within its bounds.
func TestCalcSessionLayoutGivesTheNameTheLargerShare(t *testing.T) {
	for _, width := range []int{100, 140, 200} {
		l := calcSessionLayout(width)
		if l.session <= l.project {
			t.Errorf("width %d: session=%d project=%d; the name should get more room than the project",
				width, l.session, l.project)
		}
		if l.project < minProjectWidth || l.project > maxProjectWidth {
			t.Errorf("width %d: project=%d, want between %d and %d", width, l.project, minProjectWidth, maxProjectWidth)
		}
	}
}

// Below the threshold the origin column goes, so the session name keeps room.
func TestCalcSessionLayoutOriginDropsAtBoundary(t *testing.T) {
	if l := calcSessionLayout(originColumnMinTTY); l.origin != fixedOriginWidth {
		t.Errorf("expected origin=%d at width=%d, got %d", fixedOriginWidth, originColumnMinTTY, l.origin)
	}
	if l := calcSessionLayout(originColumnMinTTY - 1); l.origin != 0 {
		t.Errorf("expected origin=0 at width=%d, got %d", originColumnMinTTY-1, l.origin)
	}
}

// On a terminal too narrow for every column, the name and project keep their
// minimums rather than shrinking to nothing; the row wraps instead of hiding
// which session it is.
func TestCalcSessionLayoutKeepsMinimumsWhenCrowded(t *testing.T) {
	l := calcSessionLayout(40)
	if l.session < minSessionWidth || l.project < minProjectWidth {
		t.Errorf("session=%d project=%d at width 40; want at least %d and %d",
			l.session, l.project, minSessionWidth, minProjectWidth)
	}
}

func TestCalcHistoryLayout_WideTerminal(t *testing.T) {
	l := calcHistoryLayout(120)

	// Project should be capped at preferred width
	if l.project != prefHistProjectWidth {
		t.Errorf("expected project=%d, got %d", prefHistProjectWidth, l.project)
	}
	// totalWidth = project + branch + startTime + duration + msgs + 4 gaps
	expected := l.project + l.branch + l.startTime + l.duration + l.msgs + 4
	if l.totalWidth != expected {
		t.Errorf("expected totalWidth=%d, got %d", expected, l.totalWidth)
	}
}

func TestTruncate_NegativeMax(t *testing.T) {
	result := truncate("hello world", -5)
	if result != "" {
		t.Errorf("expected empty string for negative max, got %q", result)
	}
}

func TestTruncate_ZeroMax(t *testing.T) {
	result := truncate("hello world", 0)
	if result != "" {
		t.Errorf("expected empty string for zero max, got %q", result)
	}
}

func TestCalcHistoryLayout_NarrowTerminal(t *testing.T) {
	l := calcHistoryLayout(60)

	// At narrow widths, project gets whatever remains (may be clamped to min)
	if l.project < minHistProjectWidth {
		t.Errorf("expected project >= %d, got %d", minHistProjectWidth, l.project)
	}
	expected := l.project + l.branch + l.startTime + l.duration + l.msgs + 4
	if l.totalWidth != expected {
		t.Errorf("expected totalWidth=%d, got %d", expected, l.totalWidth)
	}
}

// A title longer than the terminal is wide leaves no room for the trailing run.
// strings.Repeat panics on a negative count, and this divider is drawn by the
// history and usage views, so an unclamped width would take the whole TUI down
// on a narrow terminal rather than just drawing an ugly line.
func TestSectionRuleSurvivesATerminalNarrowerThanItsTitle(t *testing.T) {
	for _, width := range []int{0, 1, 5, 12} {
		var buf strings.Builder
		sectionRule(&buf, "Local Usage (5h window, Claude Code)", width, "\n")

		got := stripANSI(buf.String())
		if !strings.Contains(got, "Local Usage") {
			t.Errorf("width %d: the title was lost from %q", width, got)
		}
		if !strings.HasSuffix(got, "━\n") {
			t.Errorf("width %d: no trailing rule drawn: %q", width, got)
		}
	}
}

// The divider fills exactly the width it is given. Counting the title in bytes
// makes a multi-byte title over-subtract, so the rule comes up short and the
// section header stops lining up with the rows under it.
func TestSectionRuleFillsTheWidthWithAMultiByteTitle(t *testing.T) {
	for _, title := range []string{"Local Usage", "Использование", "日本語の見出し"} {
		const width = 60
		var buf strings.Builder
		sectionRule(&buf, title, width, "")

		got := utf8.RuneCountInString(stripANSI(buf.String()))
		if got != width {
			t.Errorf("title %q drew %d columns, want %d", title, got, width)
		}
	}
}
