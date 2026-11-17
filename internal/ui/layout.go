package ui

// Column width constraints for the session table. Columns, left to right:
// SESSION, ORIGIN, PROJECT, CONTEXT, STATUS, IDLE FOR. The session's last reply
// is rendered on a second line, so it needs no column.
const (
	fixedOriginWidth  = 18 // "Claude · Ghostty" = 16; longer origins are cut
	fixedContextWidth = 21 // progress bar (10) + " 100%" (5) + " (1M)" suffix (5) + 1 padding
	fixedStatusWidth  = 12 // "▲ Needs you" = 11 + 1 padding
	fixedIdleWidth    = 9  // "IDLE FOR" header; values like "23h 59m" fit
	minProjectWidth   = 12
	maxProjectWidth   = 28 // past this, extra width serves the session name better
	minSessionWidth   = 14
	// originColumnMinTTY is the terminal width below which the origin column is
	// dropped, so the session name keeps room to be read.
	originColumnMinTTY = 100
)

// sessionLayout holds the computed column widths for the session table.
type sessionLayout struct {
	session    int
	origin     int
	project    int
	context    int
	status     int
	idle       int
	totalWidth int
}

// calcSessionLayout computes column widths for the given terminal width.
//
// Origin, context, status and idle keep fixed widths. What remains is shared
// between the session name and the project: the project takes about a
// third, between minProjectWidth and maxProjectWidth, and the session name the
// rest, because a name is the longer and more telling of the two. The origin
// column is dropped on narrow terminals. One separator space sits between each
// pair of adjacent columns.
func calcSessionLayout(width int) sessionLayout {
	l := sessionLayout{
		context: fixedContextWidth,
		status:  fixedStatusWidth,
		idle:    fixedIdleWidth,
	}
	gaps := 4 // session|project|context|status|idle
	if width >= originColumnMinTTY {
		l.origin = fixedOriginWidth
		gaps = 5
	}

	remaining := width - (l.origin + l.context + l.status + l.idle + gaps)
	if remaining < minSessionWidth+minProjectWidth {
		remaining = minSessionWidth + minProjectWidth
	}
	l.project = remaining / 3
	if l.project < minProjectWidth {
		l.project = minProjectWidth
	}
	if l.project > maxProjectWidth {
		l.project = maxProjectWidth
	}
	l.session = remaining - l.project

	l.totalWidth = l.session + l.origin + l.project + l.context + l.status + l.idle + gaps
	return l
}

// Column width constraints for history table
const (
	minHistProjectWidth  = 15
	prefHistProjectWidth = 30
	fixedBranchWidth     = 12
	fixedHistTimeWidth   = 7 // "HH:MM" + padding
	fixedDurationWidth   = 10
	fixedMsgsWidth       = 5
)

// historyLayout holds the computed column widths for the history table.
type historyLayout struct {
	project    int
	branch     int
	startTime  int
	duration   int
	msgs       int
	totalWidth int
}

// calcHistoryLayout computes column widths for the history table.
func calcHistoryLayout(width int) historyLayout {
	l := historyLayout{
		branch:    fixedBranchWidth,
		startTime: fixedHistTimeWidth,
		duration:  fixedDurationWidth,
		msgs:      fixedMsgsWidth,
	}

	// 4 gaps between 5 columns
	const columnGaps = 4
	fixed := l.branch + l.startTime + l.duration + l.msgs + columnGaps
	remaining := width - fixed
	if remaining < minHistProjectWidth {
		remaining = minHistProjectWidth
	}
	l.project = remaining
	if l.project > prefHistProjectWidth {
		l.project = prefHistProjectWidth
	}

	l.totalWidth = l.project + l.branch + l.startTime + l.duration + l.msgs + columnGaps

	return l
}

// Column width constraints for usage table
const (
	fixedUsageInputWidth  = 10
	fixedUsageOutputWidth = 10
	fixedUsageCacheWidth  = 10
	fixedUsageTotalWidth  = 10
	minUsageProjectWidth  = 15
)

// usageLayout holds the computed column widths for the usage per-session table.
type usageLayout struct {
	project    int
	input      int
	output     int
	cache      int
	total      int
	totalWidth int
}

// calcUsageLayout computes column widths for the usage table.
func calcUsageLayout(width int) usageLayout {
	l := usageLayout{
		input:  fixedUsageInputWidth,
		output: fixedUsageOutputWidth,
		cache:  fixedUsageCacheWidth,
		total:  fixedUsageTotalWidth,
	}

	// 4 gaps between 5 columns, plus 2-char indent
	const columnGaps = 4
	const indent = 2
	fixed := l.input + l.output + l.cache + l.total + columnGaps + indent
	remaining := width - fixed
	if remaining < minUsageProjectWidth {
		remaining = minUsageProjectWidth
	}
	l.project = remaining

	l.totalWidth = l.project + l.input + l.output + l.cache + l.total + columnGaps

	return l
}

// Column width constraints for the flags feed
const (
	fixedFlagWhenWidth     = 12 // "Sep 14 10:32"
	fixedFlagSeverityWidth = 8  // "critical"
	fixedFlagRuleWidth     = 22 // the longest rule id, "protected-branch-push"
	prefFlagProjectWidth   = 28
	minFlagProjectWidth    = 12
	minFlagDetailWidth     = 20
)

// flagLayout holds the computed column widths for the flags feed.
type flagLayout struct {
	when       int
	severity   int
	project    int
	rule       int
	detail     int
	totalWidth int
}

// calcFlagLayout computes column widths for the flags feed.
//
// Unlike the other three tables, the slack goes to the *last* column: the detail
// is a command, and a command truncated at 30 characters is a command nobody can
// identify. The project column is capped so a deep monorepo path cannot spend
// the whole line.
func calcFlagLayout(width int) flagLayout {
	l := flagLayout{
		when:     fixedFlagWhenWidth,
		severity: fixedFlagSeverityWidth,
		rule:     fixedFlagRuleWidth,
		project:  prefFlagProjectWidth,
	}

	// 4 gaps between 5 columns.
	const columnGaps = 4
	fixed := l.when + l.severity + l.rule + columnGaps
	if width-fixed-l.project < minFlagDetailWidth {
		l.project = width - fixed - minFlagDetailWidth
	}
	if l.project < minFlagProjectWidth {
		l.project = minFlagProjectWidth
	}

	l.detail = width - fixed - l.project
	if l.detail < minFlagDetailWidth {
		l.detail = minFlagDetailWidth
	}

	l.totalWidth = l.when + l.severity + l.project + l.rule + l.detail + columnGaps
	return l
}
