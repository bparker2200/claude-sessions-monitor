package session

// This file is the third session producer. It reads OpenAI Codex's rollout
// files the way session.go reads Claude Code's logs and omp.go reads Oh My
// Pi's, and hands Discover the same []Session.
//
// What the format demands, as observed on Codex CLI 0.167 (it is not
// documented, and it changed between 0.163 and 0.167):
//
//   - One append-only JSONL "rollout" per session, under
//     sessions/YYYY/MM/DD/rollout-<timestamp>-<uuid>.jsonl. The folder is the
//     day the session *started*, so a long-lived session keeps writing into an
//     old day's folder, and nothing is grouped by project.
//   - Every line is {"timestamp", "type", "payload"}. The first is
//     session_meta: the session id, its working directory, and an originator
//     naming what started it ("codex-tui", "codex_exec", "Codex Desktop").
//   - Each turn starts with event_msg/task_started and ends with
//     event_msg/task_complete, or event_msg/turn_aborted when the user
//     interrupted it -- or refused an approval prompt: on disk a refusal and an
//     interrupt are the same thing.
//   - A tool call is response_item/custom_tool_call (or function_call), and its
//     result a *_output carrying the same call_id. While an approval prompt is
//     up, the call is on disk and its result is not. That dangling call is the
//     only evidence a session is waiting on the user: Codex records no
//     "approval requested" entry anywhere.
//   - turn_context records the approval policy for the turn. Under "never"
//     there is no prompt, so a dangling call is a command still running.
//   - Messages arrive as event_msg/item_completed with item.type AgentMessage
//     or UserMessage (0.167), or as event_msg agent_message / user_message
//     (older builds). Both are read.
//
// What csm deliberately does not read under ~/.codex: auth.json, the config
// files, and the SQLite databases (state, thread history, logs). The rollouts
// and session_index.jsonl hold everything shown here, and the databases would
// cost csm its single static binary.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// codexSessionsDirEnv overrides where csm looks for Codex rollouts, the same
// way CSM_OMP_SESSIONS_DIR does for omp. Without it, $CODEX_HOME/sessions is
// used, then ~/.codex/sessions: Codex itself honours CODEX_HOME.
const codexSessionsDirEnv = "CSM_CODEX_SESSIONS_DIR"

// codexHome returns Codex's home directory.
func codexHome() (string, error) {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("unable to determine home directory: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

// codexSessionsDir returns the directory Codex keeps rollouts in.
func codexSessionsDir() (string, error) {
	if dir := os.Getenv(codexSessionsDirEnv); dir != "" {
		return dir, nil
	}
	home, err := codexHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "sessions"), nil
}

// codexSessionIndexPath is the file Codex writes each session's name to.
// It sits beside the sessions directory, so an overridden sessions directory
// takes it along.
func codexSessionIndexPath() (string, error) {
	dir, err := codexSessionsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dir), "session_index.jsonl"), nil
}

// codexDesktopHosts are the accounting names of the processes Codex desktop
// sessions run inside. The desktop app is part of the ChatGPT app on current
// releases; "Codex" covers a standalone build.
var codexDesktopHosts = map[string]bool{"ChatGPT": true, "Codex": true}

// codexDesktopOrigin is the origin every desktop session carries. It is fixed
// rather than detected: there is no session process to detect it from.
var codexDesktopOrigin = Origin{Category: OriginDesktop, App: "codex-desktop", Display: "Codex Desktop"}

// isCodexDesktop reports whether a rollout's originator is the desktop app.
// Seen: "Codex Desktop", "codex_work_desktop". Terminal and headless sessions
// are "codex-tui" and "codex_exec".
func isCodexDesktop(originator string) bool {
	return strings.Contains(strings.ToLower(originator), "desktop")
}

const (
	// codexHorizon bounds which rollouts are read at all. Older ones are
	// history: a terminal session quiet for a week has not been looked at.
	codexHorizon = 7 * 24 * time.Hour
	// codexDesktopHorizon bounds which desktop sessions are listed while the
	// desktop app is open. The app keeps every conversation, and with no
	// per-session process there is no other way to tell the one you are using
	// from last month's; a day covers a session left overnight.
	codexDesktopHorizon = 24 * time.Hour
	// codexScanDays is how many day folders back the scan walks. A session
	// writes into the folder of the day it started, so this has to reach past
	// codexHorizon for long-lived sessions; past it, a session is history.
	codexScanDays = 30
	// codexKeep is how many status-relevant entries a parse keeps: enough for
	// several whole turns, which is what the status rules look at.
	codexKeep = 200
)

// codexKind is the part of a rollout line the status rules care about.
type codexKind int

const (
	codexTurnContext codexKind = iota + 1
	codexTaskStarted
	codexTaskComplete
	codexTurnAborted
	codexUserMessage
	codexAgentMessage
	codexToolCall
	codexToolOutput
)

// codexEntry is one status-relevant rollout line, already decoded. Lines the
// rules do not read (token counts, reasoning, world state) are dropped at parse
// time, so the cached parse holds only these.
type codexEntry struct {
	at     time.Time
	kind   codexKind
	callID string
	name   string // tool name, on a call
	text   string // message text, on a message or a completed task
	// policyAsks is set on a turn_context whose approval policy can put a
	// prompt in front of the user: anything but "never".
	policyAsks bool
}

// codexLine is the envelope every rollout line shares.
type codexLine struct {
	Timestamp time.Time       `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexPayload declares every payload field csm reads, across the payload
// types that carry them. A field absent from a given type stays zero.
type codexPayload struct {
	Type string `json:"type"`

	// session_meta
	ID         string `json:"id"`
	CWD        string `json:"cwd"`
	Originator string `json:"originator"`

	// turn_context. The policy is a string, or an object for "granular".
	ApprovalPolicy json.RawMessage `json:"approval_policy"`
	Model          string          `json:"model"`

	// response_item tool calls and their outputs
	Name   string `json:"name"`
	CallID string `json:"call_id"`

	// event_msg
	LastAgentMessage string     `json:"last_agent_message"`
	Message          string     `json:"message"` // user_message / agent_message, pre-0.167
	Item             *codexItem `json:"item"`
}

// codexItem is the item of an event_msg/item_completed line.
type codexItem struct {
	Type    string       `json:"type"`
	Content []codexBlock `json:"content"`
}

// codexBlock is one content block of a message item. Agent text blocks are
// typed "Text" and user ones "text"; images and files carry no text.
type codexBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// text joins an item's text blocks.
func (it *codexItem) text() string {
	var parts []string
	for _, b := range it.Content {
		if strings.EqualFold(b.Type, "text") && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, " ")
}

// codexPolicyAsks reports whether an approval policy can prompt the user.
// Only "never" cannot. The granular form is an object of per-category
// switches; it is treated as asking, because some category might.
func codexPolicyAsks(raw json.RawMessage) bool {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s != "never"
	}
	return len(raw) > 0 && string(raw) != "null"
}

// codexParsedLog holds what one pass over a rollout yields. As with the other
// harnesses' parses, it changes only when the file does.
type codexParsedLog struct {
	entries       []codexEntry
	sessionID     string
	cwd           string
	originator    string
	model         string
	lastMessage   string
	lastEntryTime time.Time
}

func (pl codexParsedLog) entryCount() int {
	// The header counts: a rollout with only session_meta is a session in its
	// first moments, not an unreadable file.
	if pl.sessionID != "" {
		return len(pl.entries) + 1
	}
	return len(pl.entries)
}

// parseCodexLogFile reads a rollout once, keeping the last keep status-relevant
// entries.
func parseCodexLogFile(logFile string, keep int) (codexParsedLog, error) {
	return parseCodexLogFileWithLimit(logFile, keep, maxLogLineBytes)
}

// parseCodexLogFileWithLimit is parseCodexLogFile with the scanner's line limit
// explicit, so a test can trigger the oversized-line path cheaply.
func parseCodexLogFileWithLimit(logFile string, keep, maxLineBytes int) (codexParsedLog, error) {
	file, err := os.Open(logFile)
	if err != nil {
		return codexParsedLog{}, err
	}
	defer func() { _ = file.Close() }()

	var pl codexParsedLog
	var entries []codexEntry
	scanner := newLogScanner(file, maxLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		e, ok := decodeCodexLine(line, &pl)
		if !ok {
			continue
		}
		entries = append(entries, e)
		// Trimmed as it goes rather than at the end: a rollout can run to tens
		// of thousands of lines, and only the tail decides anything.
		if len(entries) > 2*keep {
			entries = append(entries[:0:0], entries[len(entries)-keep:]...)
		}
	}
	if len(entries) > keep {
		entries = append([]codexEntry(nil), entries[len(entries)-keep:]...)
	}
	pl.entries = entries
	return pl, scanner.Err()
}

// decodeCodexLine decodes one rollout line. Header fields go straight into pl;
// a status-relevant line comes back as an entry. An unparseable line costs that
// line only, as on the other harnesses.
func decodeCodexLine(line []byte, pl *codexParsedLog) (codexEntry, bool) {
	var l codexLine
	if json.Unmarshal(line, &l) != nil {
		return codexEntry{}, false
	}
	// Payloads csm never reads are skipped before the second decode; token
	// counts alone are a third of a rollout's lines.
	switch l.Type {
	case "session_meta", "turn_context", "event_msg", "response_item":
	default:
		return codexEntry{}, false
	}
	var p codexPayload
	if json.Unmarshal(l.Payload, &p) != nil {
		return codexEntry{}, false
	}
	if !l.Timestamp.IsZero() && l.Type != "session_meta" {
		pl.lastEntryTime = l.Timestamp
	}

	e := codexEntry{at: l.Timestamp}
	switch l.Type {
	case "session_meta":
		// A forked session repeats its parent's header; the first is its own.
		if pl.sessionID == "" {
			pl.sessionID, pl.cwd, pl.originator = p.ID, p.CWD, p.Originator
		}
		return codexEntry{}, false
	case "turn_context":
		if p.Model != "" {
			pl.model = p.Model
		}
		if p.CWD != "" {
			pl.cwd = p.CWD
		}
		e.kind, e.policyAsks = codexTurnContext, codexPolicyAsks(p.ApprovalPolicy)
	case "response_item":
		switch p.Type {
		case "custom_tool_call", "function_call", "local_shell_call":
			e.kind, e.callID, e.name = codexToolCall, p.CallID, p.Name
		case "custom_tool_call_output", "function_call_output", "local_shell_call_output":
			e.kind, e.callID = codexToolOutput, p.CallID
		default:
			return codexEntry{}, false
		}
	case "event_msg":
		switch p.Type {
		case "task_started":
			e.kind = codexTaskStarted
		case "task_complete":
			e.kind, e.text = codexTaskComplete, p.LastAgentMessage
		case "turn_aborted":
			e.kind = codexTurnAborted
		case "user_message":
			e.kind, e.text = codexUserMessage, p.Message
		case "agent_message":
			e.kind, e.text = codexAgentMessage, p.Message
		case "item_completed":
			if p.Item == nil {
				return codexEntry{}, false
			}
			switch p.Item.Type {
			case "UserMessage":
				e.kind, e.text = codexUserMessage, p.Item.text()
			case "AgentMessage":
				e.kind, e.text = codexAgentMessage, p.Item.text()
			default:
				return codexEntry{}, false
			}
		default:
			return codexEntry{}, false
		}
	}
	if (e.kind == codexAgentMessage || e.kind == codexTaskComplete) && strings.TrimSpace(e.text) != "" {
		pl.lastMessage = e.text
	}
	return e, true
}

// codexToolLabel names what a pending tool call is doing, without its input:
// `exec`'s input is the command as code, and the label sits on the dashboard.
func codexToolLabel(name string) string {
	switch name {
	case "exec", "exec_command", "shell", "local_shell":
		return "Running a command"
	case "apply_patch":
		return "Applying a patch"
	case "":
		return "Using a tool"
	}
	return "Using: " + name
}

// determineCodexStatus decides a Codex session's status, with the same four
// values and the same windows as the other harnesses.
//
// The rules, in evaluation order:
//
//   - Not running -> Inactive. Running with no entries -> Waiting.
//   - A tool call with no result, in a turn that has not ended:
//     under approval policy "never" there is no prompt, so it is a command
//     still running -> Working, however long. Under any other policy it is
//     Working while younger than recentActivityWindow and Needs Input after.
//     This is the only producer of Needs Input.
//   - The newest turn ended (task_complete or turn_aborted after the newest
//     task_started) -> Waiting, unless a newer prompt is within the window.
//   - Otherwise the shared ageStatus rules.
func determineCodexStatus(entries []codexEntry, isRunning bool, fileModTime time.Time) (Status, string) {
	if !isRunning {
		return StatusInactive, "-"
	}
	if len(entries) == 0 {
		return StatusWaiting, "-"
	}

	var (
		policyAsks                                             bool
		open                                                   []codexEntry
		lastStart, lastEnd, lastAgent, lastUser, lastTimestamp time.Time
		lastCall                                               string
	)
	for _, e := range entries {
		if e.at.After(lastTimestamp) {
			lastTimestamp = e.at
		}
		switch e.kind {
		case codexTurnContext:
			policyAsks = e.policyAsks
		case codexTaskStarted:
			lastStart = e.at
		case codexTaskComplete, codexTurnAborted:
			// A turn's end settles every call it left open, answered or not.
			lastEnd, open = e.at, nil
			if e.kind == codexTaskComplete && e.text != "" {
				lastAgent = e.at
			}
		case codexToolCall:
			open = append(open, e)
			lastCall = e.name
		case codexToolOutput:
			for i := range open {
				if open[i].callID == e.callID {
					open = append(open[:i], open[i+1:]...)
					break
				}
			}
		case codexAgentMessage:
			lastAgent = e.at
		case codexUserMessage:
			lastUser = e.at
		}
	}

	// The oldest open call is what the session is blocked on.
	if len(open) > 0 {
		pending := open[0]
		task := codexToolLabel(pending.name)
		if !policyAsks || time.Since(pending.at) < recentActivityWindow {
			return StatusWorking, task
		}
		return StatusNeedsInput, task
	}

	if !lastEnd.IsZero() && !lastEnd.Before(lastStart) {
		if lastUser.After(lastEnd) && time.Since(lastUser) < recentActivityWindow {
			return StatusWorking, "Processing..."
		}
		return StatusWaiting, "-"
	}

	var lastPrompt time.Time
	if lastUser.After(lastAgent) {
		lastPrompt = lastUser
	}
	return ageStatus(activity{
		fileModTime:   fileModTime,
		lastEntry:     lastTimestamp,
		lastAssistant: lastAgent,
		lastPrompt:    lastPrompt,
		task: func() string {
			if lastCall != "" {
				return codexToolLabel(lastCall)
			}
			return "Thinking"
		},
	})
}

// codexSessionIDFromLogFile returns the session id from a rollout's name,
// rollout-<timestamp>-<uuid>.jsonl, for a rollout whose header could not be
// read. The uuid is the last 36 characters.
func codexSessionIDFromLogFile(logFile string) string {
	stem := strings.TrimSuffix(filepath.Base(logFile), ".jsonl")
	if len(stem) >= 36 {
		return stem[len(stem)-36:]
	}
	return stem
}

// listCodexRollouts returns the rollouts modified within codexHorizon, newest
// first, from the last codexScanDays day folders and from any flat layout an
// older build left at the top of the sessions directory.
//
// Day folders are walked by name, not by mtime: a folder's mtime moves when a
// rollout is created in it, not when one is appended to, so it says nothing
// about whether a long-lived session in it is still writing.
func listCodexRollouts(dir string, now time.Time) []logCandidate {
	var logs []logCandidate
	add := func(d string) {
		entries, err := os.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), "rollout-") || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil || now.Sub(info.ModTime()) > codexHorizon {
				continue
			}
			logs = append(logs, logCandidate{path: filepath.Join(d, e.Name()), modTime: info.ModTime(), size: info.Size()})
		}
	}
	add(dir)
	for i := 0; i < codexScanDays; i++ {
		day := now.AddDate(0, 0, -i)
		add(filepath.Join(dir, day.Format("2006"), day.Format("01"), day.Format("02")))
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i].modTime.After(logs[j].modTime) })
	return logs
}

// codexNames caches session_index.jsonl, which Codex appends a line to each
// time a session is named or renamed. It is read only when it changes.
var codexNames struct {
	sync.Mutex
	modTime time.Time
	size    int64
	byID    map[string]string
}

// codexSessionNames returns session id -> name. The newest line for an id
// wins, which is what a rename leaves behind.
func codexSessionNames() map[string]string {
	path, err := codexSessionIndexPath()
	if err != nil {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}

	codexNames.Lock()
	defer codexNames.Unlock()
	if codexNames.byID != nil && codexNames.size == info.Size() && codexNames.modTime.Equal(info.ModTime()) {
		return codexNames.byID
	}

	f, err := os.Open(path)
	if err != nil {
		return codexNames.byID
	}
	defer func() { _ = f.Close() }()
	byID := make(map[string]string)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var row struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) == nil && row.ID != "" && row.ThreadName != "" {
			byID[row.ID] = row.ThreadName
		}
	}
	codexNames.byID, codexNames.modTime, codexNames.size = byID, info.ModTime(), info.Size()
	return byID
}

// discoverCodex returns the Codex sessions worth showing, given the process
// scan Discover already made. A missing sessions directory is not an error.
//
// Pairing, which Codex gives no help with -- it writes no pid anywhere:
//
//   - Terminal and headless sessions are matched to a `codex` process whose
//     working directory is the session's, the way Claude Code was paired
//     before its registry existed. With N such processes in a folder, the N
//     newest rollouts there are running. The rollout file itself cannot be
//     used: a terminal session often hands it to the shared `codex app-server`,
//     which holds it, and the lock beside it, after the terminal has quit.
//   - Desktop sessions have no process of their own. They count as running
//     while the desktop app is, and are listed only within
//     codexDesktopHorizon.
func discoverCodex(procs []harnessProcess, liveFiles map[string]struct{}) []Session {
	dir, err := codexSessionsDir()
	if err != nil {
		return nil
	}
	now := time.Now()
	logs := listCodexRollouts(dir, now)
	if len(logs) == 0 {
		return nil
	}

	pidsByCWD := pidsByDir(procs, HarnessCodex, func(cwd string) string { return cwd })
	procByPID := procsByPID(procs)
	desktopOpen := false
	for _, p := range procs {
		if p.desktopHost {
			desktopOpen = true
			break
		}
	}
	names := codexSessionNames()

	type rollout struct {
		logCandidate
		pl  codexParsedLog
		err error
	}
	byCWD := make(map[string][]rollout)
	var cwdOrder []string
	var desktop []rollout
	for _, l := range logs {
		pl, err := cachedParseCodexLogFile(l.path, l.modTime, l.size)
		if err != nil && pl.entryCount() == 0 {
			continue
		}
		r := rollout{logCandidate: l, pl: pl, err: err}
		if isCodexDesktop(pl.originator) {
			desktop = append(desktop, r)
			continue
		}
		if _, seen := byCWD[pl.cwd]; !seen {
			cwdOrder = append(cwdOrder, pl.cwd)
		}
		byCWD[pl.cwd] = append(byCWD[pl.cwd], r)
	}

	var sessions []Session
	for _, cwd := range cwdOrder {
		group := byCWD[cwd]
		pids := pidsByCWD[cwd]
		if cwd == "" {
			pids = nil // a rollout with no folder cannot be placed
		}
		candidates := make([]logCandidate, len(group))
		for i, r := range group {
			candidates[i] = r.logCandidate
		}
		selected := selectActiveLogs(candidates, len(pids))
		for i, path := range selected {
			var r rollout
			for _, g := range group {
				if g.path == path {
					r = g
					break
				}
			}
			running := i < len(pids)
			pid := 0
			if running {
				pid = pids[i]
			}
			confident := len(pids) == 1 && len(selected) == 1
			liveFiles[r.path] = struct{}{}
			sessions = append(sessions, buildCodexSession(r.logCandidate, r.pl, r.err, running, pid, confident,
				procByPID[pid].orphan, names, false))
		}
	}

	for _, r := range desktop {
		if now.Sub(r.modTime) > codexDesktopHorizon {
			continue
		}
		liveFiles[r.path] = struct{}{}
		sessions = append(sessions, buildCodexSession(r.logCandidate, r.pl, r.err, desktopOpen, 0, false, false, names, true))
	}
	return sessions
}

// codexInternalFolder reports whether a working directory is one the Codex
// desktop app made for a ChatGPT project (~/.codex/.chatgpt-projects/g-p-<hex>).
// Its name is an id, and the project's real name is not on disk -- the folder's
// metadata lists files only -- so the project cell is left blank rather than
// showing the id.
func codexInternalFolder(cwd string) bool {
	return strings.Contains(filepath.ToSlash(cwd), "/.chatgpt-projects/")
}

// buildCodexSession turns one parsed rollout into a Session.
//
// Deliberately left zero: ContextPercent, ContextTokens and ContextWindow --
// Codex records token counts, but the percentage its own footer shows is not
// the plain ratio of them (4% on screen against 8.1% computed, measured once),
// and a number that disagrees with Codex reads as a measurement. GitBranch --
// the header records the branch the session started on, which is not
// necessarily the one it is on. Flags and Subagents have no reader yet.
func buildCodexSession(l logCandidate, pl codexParsedLog, parseErr error, running bool, pid int, confident, orphaned bool,
	names map[string]string, desktop bool) Session {
	id := pl.sessionID
	if id == "" {
		id = codexSessionIDFromLogFile(l.path)
	}
	s := Session{
		Project:      extractProjectName(pl.cwd),
		CWD:          pl.cwd,
		LogFile:      l.path,
		Status:       StatusInactive,
		SessionID:    id,
		Harness:      HarnessCodex,
		Model:        pl.model,
		LastMessage:  pl.lastMessage,
		SessionTitle: names[id],
		LastActivity: l.modTime,
	}
	if pl.cwd == "" || codexInternalFolder(pl.cwd) {
		// Not a project anyone named: see codexInternalFolder.
		s.Project = "—"
	}
	if !pl.lastEntryTime.IsZero() {
		s.LastActivity = pl.lastEntryTime
	}
	if parseErr != nil {
		s.Degraded = parseErr.Error()
	}
	if desktop {
		s.Origin = codexDesktopOrigin
	} else {
		s.Origin = resolveOrigin(id, running, pid)
	}
	if running && pid > 0 {
		s.GhostPID = pid
	}
	s.PIDConfident = confident && s.GhostPID > 0
	s.Status, s.Task = determineCodexStatus(pl.entries, running, l.modTime)
	s.IsGhost = isGhost(running, orphaned, s.LastActivity)
	return s
}
