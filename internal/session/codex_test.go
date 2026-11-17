package session

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every fixture here is written by hand from the field names Codex uses. None
// is copied from a real rollout: those hold whatever the user typed.

// codexRec builds one rollout line.
func codexRec(t *testing.T, at time.Time, typ string, payload map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"timestamp": at.UTC().Format(time.RFC3339Nano),
		"type":      typ,
		"payload":   payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func codexMeta(t *testing.T, at time.Time, id, cwd, originator string) string {
	return codexRec(t, at, "session_meta", map[string]any{"id": id, "cwd": cwd, "originator": originator})
}

func codexTurn(t *testing.T, at time.Time, policy any) string {
	return codexRec(t, at, "turn_context", map[string]any{"approval_policy": policy, "model": "gpt-test", "cwd": "/home/u/proj"})
}

func codexEvent(t *testing.T, at time.Time, typ string, extra map[string]any) string {
	p := map[string]any{"type": typ}
	for k, v := range extra {
		p[k] = v
	}
	return codexRec(t, at, "event_msg", p)
}

func codexCall(t *testing.T, at time.Time, id string) string {
	return codexRec(t, at, "response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": id, "input": "ignored"})
}

func codexOutput(t *testing.T, at time.Time, id string) string {
	return codexRec(t, at, "response_item", map[string]any{"type": "custom_tool_call_output", "call_id": id, "output": "ignored"})
}

func codexAgentItem(t *testing.T, at time.Time, text string) string {
	return codexEvent(t, at, "item_completed", map[string]any{
		"item": map[string]any{"type": "AgentMessage", "content": []map[string]any{{"type": "Text", "text": text}}},
	})
}

func codexUserItem(t *testing.T, at time.Time, text string) string {
	return codexEvent(t, at, "item_completed", map[string]any{
		"item": map[string]any{"type": "UserMessage", "content": []map[string]any{{"type": "text", "text": text}}},
	})
}

// parseCodexLines writes lines to a temp rollout and parses it.
func parseCodexLines(t *testing.T, lines []string) codexParsedLog {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout-2026-11-16T10-00-00-11111111-2222-3333-4444-555555555555.jsonl")
	writeLines(t, path, lines)
	pl, err := parseCodexLogFile(path, codexKeep)
	if err != nil {
		t.Fatalf("parseCodexLogFile: %v", err)
	}
	return pl
}

// The header and the last reply are what the row shows: the folder becomes the
// project, the originator decides desktop or terminal, and the final reply is
// the "last turn" line.
func TestParseCodexReadsTheHeaderAndTheLastReply(t *testing.T) {
	t0 := time.Now().Add(-10 * time.Minute)
	pl := parseCodexLines(t, []string{
		codexMeta(t, t0, "sess-1", "/home/u/proj", "codex-tui"),
		codexEvent(t, t0, "task_started", nil),
		codexTurn(t, t0, "never"),
		codexRec(t, t0, "token_count", map[string]any{"info": nil}),
		codexUserItem(t, t0, "make the file"),
		codexCall(t, t0.Add(time.Second), "c1"),
		codexOutput(t, t0.Add(2*time.Second), "c1"),
		codexAgentItem(t, t0.Add(3*time.Second), "Working on it."),
		codexEvent(t, t0.Add(4*time.Second), "task_complete", map[string]any{"last_agent_message": "Done. The file exists."}),
	})
	if pl.sessionID != "sess-1" || pl.cwd != "/home/u/proj" || pl.originator != "codex-tui" || pl.model != "gpt-test" {
		t.Errorf("header = id %q cwd %q originator %q model %q; the row loses its project or its origin",
			pl.sessionID, pl.cwd, pl.originator, pl.model)
	}
	if pl.lastMessage != "Done. The file exists." {
		t.Errorf("last message = %q, want the final reply", pl.lastMessage)
	}
	if !pl.lastEntryTime.Equal(t0.Add(4 * time.Second).UTC().Truncate(time.Nanosecond)) {
		t.Errorf("last entry time = %v, want the task_complete's", pl.lastEntryTime)
	}
}

// Builds before 0.167 wrote messages as event_msg user_message/agent_message.
// A session started on one is still a session.
func TestParseCodexReadsTheOlderMessageEvents(t *testing.T) {
	t0 := time.Now()
	pl := parseCodexLines(t, []string{
		codexMeta(t, t0, "sess-old", "/home/u/proj", "codex-tui"),
		codexEvent(t, t0, "user_message", map[string]any{"message": "hello"}),
		codexEvent(t, t0, "agent_message", map[string]any{"message": "Hi there."}),
	})
	if pl.lastMessage != "Hi there." {
		t.Errorf("last message = %q; an older build's reply is lost", pl.lastMessage)
	}
	if len(pl.entries) != 2 || pl.entries[0].kind != codexUserMessage {
		t.Errorf("entries = %+v, want a user message then an agent message", pl.entries)
	}
}

// The status rules, against the behaviour measured on Codex 0.167 with
// `codex -a on-request`: the pending call sits on disk with no result for as
// long as the prompt is up, and a refusal records the result followed by
// turn_aborted.
func TestDetermineCodexStatus(t *testing.T) {
	now := time.Now()
	old := now.Add(-3 * time.Minute)
	fresh := now.Add(-20 * time.Second)
	entries := func(lines ...string) []codexEntry {
		head := codexMeta(t, now.Add(-time.Hour), "s", "/home/u/proj", "codex-tui")
		return parseCodexLines(t, append([]string{head}, lines...)).entries
	}

	for _, tc := range []struct {
		name    string
		entries []codexEntry
		running bool
		want    Status
		task    string
		why     string
	}{
		{
			name:    "prompt up for three minutes",
			entries: entries(codexEvent(t, old, "task_started", nil), codexTurn(t, old, "on-request"), codexCall(t, old, "c1")),
			running: true, want: StatusNeedsInput, task: "Running a command",
			why: "a session blocked on your approval must turn red",
		},
		{
			name:    "call just made",
			entries: entries(codexEvent(t, fresh, "task_started", nil), codexTurn(t, fresh, "on-request"), codexCall(t, fresh, "c1")),
			running: true, want: StatusWorking,
			why: "a call younger than the window is still running, not a prompt",
		},
		{
			name:    "approvals off, long command",
			entries: entries(codexEvent(t, old, "task_started", nil), codexTurn(t, old, "never"), codexCall(t, old.Add(-10*time.Minute), "c1")),
			running: true, want: StatusWorking,
			why: "under approval policy never there is no prompt, so a slow command must never read as needing you",
		},
		{
			name:    "granular policy",
			entries: entries(codexEvent(t, old, "task_started", nil), codexTurn(t, old, map[string]any{"granular": map[string]any{"sandbox_approval": true}}), codexCall(t, old, "c1")),
			running: true, want: StatusNeedsInput,
			why: "a granular policy can still prompt",
		},
		{
			name: "prompt refused with Esc",
			entries: entries(codexEvent(t, old, "task_started", nil), codexTurn(t, old, "on-request"), codexCall(t, old, "c1"),
				codexOutput(t, fresh, "c1"), codexEvent(t, fresh, "turn_aborted", map[string]any{"reason": "interrupted"})),
			running: true, want: StatusWaiting,
			why: "a refused prompt leaves the session idle, not still waiting",
		},
		{
			name: "turn finished",
			entries: entries(codexEvent(t, old, "task_started", nil), codexTurn(t, old, "never"), codexCall(t, old, "c1"),
				codexOutput(t, old, "c1"), codexEvent(t, old, "task_complete", map[string]any{"last_agent_message": "Done."})),
			running: true, want: StatusWaiting,
			why: "a finished turn is idle",
		},
		{
			name:    "process gone mid-prompt",
			entries: entries(codexEvent(t, old, "task_started", nil), codexTurn(t, old, "on-request"), codexCall(t, old, "c1")),
			running: false, want: StatusInactive,
			why: "a session killed while a prompt was up must not show as needing you forever",
		},
		{
			name:    "header only",
			entries: entries(),
			running: true, want: StatusWaiting,
			why: "a session in its first moments has nothing to be working on",
		},
	} {
		got, task := determineCodexStatus(tc.entries, tc.running, now.Add(-time.Hour))
		if got != tc.want {
			t.Errorf("%s: status %q, want %q: %s", tc.name, got, tc.want, tc.why)
		}
		if tc.task != "" && task != tc.task {
			t.Errorf("%s: task %q, want %q", tc.name, task, tc.task)
		}
	}
}

// The shared background server and its helpers are not sessions. Counting the
// server would pair it to a rollout by folder and report a closed session as
// running.
func TestClassifyCodexProcesses(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want Harness
	}{
		{[]string{"/opt/homebrew/bin/codex"}, HarnessCodex},
		{[]string{"codex", "-a", "on-request"}, HarnessCodex},
		{[]string{"codex", "exec", "--json"}, HarnessCodex},
		{[]string{"codex", "app-server"}, HarnessNone},
		{[]string{"/Applications/ChatGPT.app/Contents/Resources/codex", "-c", "k=v", "app-server"}, HarnessNone},
		{[]string{"codex", "mcp-server"}, HarnessNone},
	} {
		if got := classifyProcess(tc.argv); got != tc.want {
			t.Errorf("classifyProcess(%q) = %q, want %q", tc.argv, got, tc.want)
		}
	}
	if harnessCandidate("codex-code-mode-host") {
		t.Error("codex-code-mode-host passed the prefilter; it is a helper, not a session")
	}
	if !harnessCandidate("codex") {
		t.Error("codex failed the prefilter, so no Codex session is ever found")
	}
}

// The desktop app's process is recognised by name and needs no folder lookup:
// it has none, and looking one up costs an lsof spawn on macOS.
func TestScanRecognisesTheCodexDesktopHost(t *testing.T) {
	fakeProcesses(t, []procInfo{
		{pid: 501, ppid: 1, comm: "ChatGPT"},
		{pid: 502, ppid: 900, comm: "codex"},
	})
	calls := countCwdLookups(t, func(int) (string, error) { return fakeCwd, nil })
	swapArgvLookup(t, func(pid int) ([]string, error) {
		if pid == 502 {
			return []string{"/opt/homebrew/bin/codex"}, nil
		}
		return nil, fs.ErrNotExist
	})

	procs, _, _, err := getRunningHarnessProcs()
	if err != nil {
		t.Fatalf("getRunningHarnessProcs: %v", err)
	}
	byPID := procsByPID(procs)
	if !byPID[501].desktopHost || byPID[501].harness != HarnessCodex {
		t.Errorf("ChatGPT process = %+v; desktop sessions would never read as open", byPID[501])
	}
	if calls[501] != 0 {
		t.Error("the desktop host's folder was looked up; it has none")
	}
	if byPID[502].harness != HarnessCodex || byPID[502].cwd != fakeCwd {
		t.Errorf("terminal codex = %+v, want a Codex session process placed in its folder", byPID[502])
	}
	if dirs := pidsByDir(procs, HarnessCodex, func(c string) string { return c }); len(dirs[""]) != 0 {
		t.Error("the desktop host was keyed to a folder, so it could pair with a terminal session")
	}
}

// codexStore lays out a fake ~/.codex with the given rollouts and session
// names, and points discovery at it.
type codexFixture struct {
	id, cwd, originator string
	age                 time.Duration // how long ago the rollout was last written
	lines               func(at time.Time) []string
}

func codexStore(t *testing.T, fixtures []codexFixture, names map[string]string) {
	t.Helper()
	resetParseCache()
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "codex")
	sessions := filepath.Join(root, "sessions")
	t.Setenv(codexSessionsDirEnv, sessions)
	now := time.Now()
	for _, f := range fixtures {
		at := now.Add(-f.age)
		day := filepath.Join(sessions, at.Format("2006"), at.Format("01"), at.Format("02"))
		if err := os.MkdirAll(day, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(day, "rollout-"+at.Format("2006-01-02T15-04-05")+"-"+f.id+".jsonl")
		lines := []string{codexMeta(t, at.Add(-time.Minute), f.id, f.cwd, f.originator)}
		if f.lines != nil {
			lines = append(lines, f.lines(at)...)
		}
		writeLines(t, path, lines)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	var index []string
	for id, name := range names {
		b, _ := json.Marshal(map[string]string{"id": id, "thread_name": name, "updated_at": "x"})
		index = append(index, string(b))
	}
	writeLines(t, filepath.Join(root, "session_index.jsonl"), index)
}

func codexByID(sessions []Session) map[string]Session {
	out := make(map[string]Session)
	for _, s := range sessions {
		out[s.SessionID] = s
	}
	return out
}

// One terminal process in a folder pairs with that folder's newest rollout,
// confidently, and an older finished one beside it is not running. A folder
// with no process is not running. A desktop session is open while the desktop
// app is, and is labelled as the desktop app rather than detected.
func TestDiscoverCodexPairsSessionsToProcesses(t *testing.T) {
	finished := func(at time.Time) []string {
		return []string{codexEvent(t, at, "task_complete", map[string]any{"last_agent_message": "Done."})}
	}
	codexStore(t, []codexFixture{
		{id: "live-1", cwd: "/home/u/a", originator: "codex-tui", age: time.Minute, lines: finished},
		{id: "old-1", cwd: "/home/u/a", originator: "codex-tui", age: 2 * time.Hour, lines: finished},
		{id: "other-1", cwd: "/home/u/b", originator: "codex-tui", age: 5 * time.Minute, lines: finished},
		{id: "desk-1", cwd: "/home/u/c", originator: "Codex Desktop", age: 10 * time.Minute, lines: finished},
		{id: "desk-old", cwd: "/home/u/c", originator: "Codex Desktop", age: 3 * 24 * time.Hour, lines: finished},
	}, map[string]string{"live-1": "Header cleanup", "desk-1": "Weekly review"})

	procs := []harnessProcess{
		{pid: 700, harness: HarnessCodex, cwd: "/home/u/a"},
		{pid: 701, harness: HarnessCodex, desktopHost: true},
	}
	got := codexByID(discoverCodex(procs, map[string]struct{}{}))

	live := got["live-1"]
	if live.Status != StatusWaiting || live.GhostPID != 700 || !live.PIDConfident {
		t.Errorf("live session = status %q pid %d confident %v; want Waiting, paired to 700, confidently",
			live.Status, live.GhostPID, live.PIDConfident)
	}
	if live.SessionTitle != "Header cleanup" || live.Harness != HarnessCodex || live.Project == "" {
		t.Errorf("live session = title %q harness %q project %q; the row loses its name or agent",
			live.SessionTitle, live.Harness, live.Project)
	}
	if live.LastMessage != "Done." {
		t.Errorf("live session last message = %q, want the final reply", live.LastMessage)
	}
	if s, ok := got["old-1"]; ok && s.Status != StatusInactive {
		t.Errorf("an older rollout in the same folder reads %q; one process cannot be two sessions", s.Status)
	}
	if got["other-1"].Status != StatusInactive {
		t.Errorf("a folder with no codex process reads %q; a closed session would stay on the list", got["other-1"].Status)
	}
	desk := got["desk-1"]
	if desk.Status != StatusWaiting || desk.Origin != codexDesktopOrigin || desk.SessionTitle != "Weekly review" {
		t.Errorf("desktop session = status %q origin %+v title %q; want Waiting, Codex Desktop, its name",
			desk.Status, desk.Origin, desk.SessionTitle)
	}
	if _, ok := got["desk-old"]; ok {
		t.Error("a desktop session idle for three days is listed; every old conversation would crowd the dashboard")
	}
	for id, s := range got {
		if s.ContextPercent != 0 || s.ContextTokens != 0 {
			t.Errorf("%s shows a context figure; Codex's own figure is not the plain ratio, so it must stay blank", id)
		}
	}
}

// With the desktop app closed, its sessions are not open.
func TestDiscoverCodexDesktopSessionsCloseWithTheApp(t *testing.T) {
	codexStore(t, []codexFixture{
		{id: "desk-1", cwd: "/home/u/c", originator: "codex_work_desktop", age: time.Minute},
	}, nil)
	got := codexByID(discoverCodex(nil, map[string]struct{}{}))
	if got["desk-1"].Status != StatusInactive {
		t.Errorf("desktop session reads %q with the app closed", got["desk-1"].Status)
	}
}

// No Codex installed is the common case and costs nothing.
func TestDiscoverCodexWithNoStore(t *testing.T) {
	t.Setenv(codexSessionsDirEnv, filepath.Join(t.TempDir(), "missing"))
	if got := discoverCodex(nil, map[string]struct{}{}); len(got) != 0 {
		t.Errorf("got %d sessions from a store that does not exist", len(got))
	}
}

// A rename appends a new line to the index; the newest name is the one shown.
func TestCodexSessionNamesTakeTheLatestRename(t *testing.T) {
	resetParseCache()
	root := t.TempDir()
	t.Setenv(codexSessionsDirEnv, filepath.Join(root, "sessions"))
	writeLines(t, filepath.Join(root, "session_index.jsonl"), []string{
		`{"id":"s1","thread_name":"First name","updated_at":"a"}`,
		`not json`,
		`{"id":"s1","thread_name":"Renamed","updated_at":"b"}`,
	})
	if got := codexSessionNames()["s1"]; got != "Renamed" {
		t.Errorf("name = %q, want the latest rename", got)
	}
}

func TestCodexSessionIDFromLogFile(t *testing.T) {
	got := codexSessionIDFromLogFile("/x/rollout-2026-11-16T19-52-56-01a1e7a2-ea8f-77b2-b7f2-f182db7d1844.jsonl")
	if got != "01a1e7a2-ea8f-77b2-b7f2-f182db7d1844" {
		t.Errorf("id = %q", got)
	}
	if !strings.HasPrefix(codexSessionIDFromLogFile("/x/short.jsonl"), "short") {
		t.Error("a short name should come back whole")
	}
}
