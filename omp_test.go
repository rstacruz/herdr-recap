package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const ompNative = "01a1374c-630d-75c9-a953-0e6ad3501406"

// ompSaid is an entry of a conversation, past a session's header
const ompSaid = `{"type":"message","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`

// ompSession writes a session file as omp writes one: a title slot, the
// session's record, then whatever entries
func ompSession(t *testing.T, dir, id, cwd string, entries ...string) string {
	t.Helper()
	lines := []string{
		`{"type":"title","v":1,"title":""}`,
		`{"type":"session","version":3,"id":"` + id + `","cwd":"` + cwd + `"}`,
	}
	path := filepath.Join(dir, "2026-10-13T22-05-15-405Z_"+id+".jsonl")
	write(t, path, strings.Join(append(lines, entries...), "\n")+"\n")
	return path
}

// withOmpPane stands in a pane whose foreground processes are these.
func withOmpPane(t *testing.T, procs []process, env map[string]string) {
	t.Helper()
	oldProcs, oldEnv := paneProcs, processEnv
	paneProcs = func(string) ([]process, error) { return procs, nil }
	processEnv = func(int) (map[string]string, error) { return env, nil }
	t.Cleanup(func() { paneProcs, processEnv = oldProcs, oldEnv })
}

func TestOmpSessionID(t *testing.T) {
	path := "/home/me/.omp/agent/sessions/-dev-repo/2026-10-13T22-05-15-405Z_" + ompNative
	if got := ompSessionID(path + ".jsonl"); got != ompNative {
		t.Errorf("%q: %q, want %q", path, got, ompNative)
	}
	for _, path := range []string{
		path + "-and-more.jsonl",
		"/home/me/.omp/agent/sessions/-dev-repo/" + ompNative + ".jsonl",
		"/home/me/notes/session.jsonl",
		"",
	} {
		if got := ompSessionID(path); got != "" {
			t.Errorf("%q: %q, want no session", path, got)
		}
	}
}

func TestAnOmpSessionIsFoundFromHerdrsPath(t *testing.T) {
	binDir := t.TempDir()
	write(t, filepath.Join(binDir, "omp"), "#!/bin/sh\n")
	os.Chmod(filepath.Join(binDir, "omp"), 0o755)
	withOmpPane(t, []process{
		{PID: 10, Name: "zsh"}, // the shell it was started from
		{PID: 11, Name: "omp", Argv: []string{"omp"}},
	}, map[string]string{"PATH": binDir})
	path := ompSession(t, t.TempDir(), ompNative, "/repo", ompSaid)
	a := agentInfo{PaneID: "w1:p1", Agent: "omp", Cwd: "/from-herdr",
		AgentSession: agentSession{Source: "herdr:omp", Kind: "path", Value: path}}
	s, err := resolveAgentSession(context.Background(), withDefaults(config{}), a)
	if err != nil {
		t.Fatal(err)
	}
	// The session's own record has the id and the cwd the fork keeps, not
	// herdr's
	if !s.Native || s.Agent != "omp" || s.ID != ompNative || s.Transcript != path || s.Cwd != "/repo" {
		t.Errorf("%+v", s)
	}
	// The recap runs as the pane's omp does
	if s.Path != binDir || s.Binary != filepath.Join(binDir, "omp") {
		t.Errorf("%+v", s)
	}
}

func TestAnOmpPaneWithNothingSaidHasNoTranscript(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	withOmpPane(t, []process{{PID: 11, Name: "omp"}}, nil)
	dir := t.TempDir()
	a := agentInfo{PaneID: "w1:p1", Agent: "omp",
		AgentSession: agentSession{Kind: "path", Value: ompSession(t, dir, ompNative, "/repo")}}
	s, err := resolveOmpSession(a)
	if err != nil || s.Transcript != "" || s.ID != ompNative {
		t.Fatalf("%+v %v", s, err)
	}
	if _, _, err := ensureRecap(context.Background(), withDefaults(config{}), s, "", false); !errors.Is(err, errNothingYet) {
		t.Errorf("recapped nothing: %v", err)
	}
	// A pane whose session file isn't written yet is the same.
	a.AgentSession.Value = filepath.Join(dir, "2026-10-13T22-05-15-405Z_"+ompNative+".jsonl")
	if s, err := resolveOmpSession(a); err != nil || s.Transcript != "" {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestOmpIsRecappedOnlyWhenHerdrHasItsPath(t *testing.T) {
	done := agentInfo{Agent: "omp", Status: "done",
		AgentSession: agentSession{Source: "herdr:omp", Kind: "path", Value: "/sessions/x.jsonl"}}
	if !recapCapable(withDefaults(config{}), done) || !waiting(withDefaults(config{}), done) {
		t.Error("omp, without being asked for, isn't recapped")
	}
	if recapCapable(withDefaults(config{Agents: []string{"claude", "codex"}}), done) {
		t.Error("omp, left out of agents, is recapped")
	}
	byID := done
	byID.AgentSession.Kind, byID.AgentSession.Value = "id", ompNative
	if recapCapable(withDefaults(config{}), byID) {
		t.Error("omp, without the path herdr has for it")
	}
}

func TestOmpArgsForkWithoutToolsOrWriting(t *testing.T) {
	args := ompArgs("/sessions/x.jsonl", "/state/fork/run-1", "/state/omp-fork.yml", ompPrompt)
	for _, want := range []string{"--fork", "/sessions/x.jsonl", "--session-dir", "/state/fork/run-1",
		"--config", "/state/omp-fork.yml", "--no-tools", "-p", ompPrompt} {
		if !slices.Contains(args, want) {
			t.Errorf("args lack %q: %v", want, args)
		}
	}
	if got := ompBinary(claudeSession{}); got != "omp" {
		t.Errorf("binary %q", got)
	}
}

func TestOmpRecapsItselfOnceWhileItsSessionStandsStill(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	calls := 0
	oldOmp, oldCodex, oldClaude := runOmp, runCodex, runCommand
	runOmp = func(_ context.Context, _ claudeSession, prompt string) (string, error) {
		if prompt != ompPrompt {
			t.Errorf("the fork was asked %q", prompt)
		}
		calls++
		return "Shipping the omp recap.\nNext, open the PR.\n", nil
	}
	runCodex = func(context.Context, config, claudeSession, string) (string, error) {
		t.Error("codex was run on an omp session")
		return "", errors.New("no")
	}
	runCommand = func(context.Context, config, claudeSession, []string) ([]byte, error) {
		t.Error("claude was run on an omp session")
		return nil, errors.New("no")
	}
	t.Cleanup(func() { runOmp, runCodex, runCommand = oldOmp, oldCodex, oldClaude })
	path := ompSession(t, t.TempDir(), ompNative, "/repo", ompSaid)
	s := claudeSession{Agent: "omp", Native: true, ID: ompNative, Transcript: path}
	cfg := withDefaults(config{})
	// A slug for a title: omp titles are none of this plugin's business
	r, ran, err := ensureRecap(context.Background(), cfg, s, "a-slug-for-a-title", false)
	if err != nil || !ran || r.Text != "Shipping the omp recap. Next, open the PR." || r.Source != "omp" {
		t.Fatalf("%+v %v %v", r, ran, err)
	}
	if _, ran, _ := ensureRecap(context.Background(), cfg, s, "", false); ran || calls != 1 {
		t.Errorf("recapped again: %d calls", calls)
	}
	// Anything added puts it out of date: the file isn't read for whether it
	// was conversation, as a Codex one isn't
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(ompSaid + "\n")
	f.Close()
	if _, ran, _ := ensureRecap(context.Background(), cfg, s, "", false); !ran || calls != 2 {
		t.Errorf("not recapped after the session moved on: %d calls", calls)
	}
}

func TestAnOmpForkAnswerThatsAToolCallIsntARecap(t *testing.T) {
	old := runOmp
	t.Cleanup(func() { runOmp = old })
	for _, answer := range []string{
		"<｜｜DSML｜｜ calls>\n<｜｜DSML｜｜ invoke name=\"bash\">\n",
		"<｜DSML｜tool_calls>\n<｜DSML｜invoke name=\"bash\">\n",
		"<|DSML|tool_calls>\n",
	} {
		runOmp = func(context.Context, claudeSession, string) (string, error) { return answer, nil }
		if _, err := runOmpRecap(context.Background(), withDefaults(config{}), claudeSession{ID: ompNative}); err == nil ||
			!strings.Contains(err.Error(), "tool call") {
			t.Errorf("a tool call was taken as a recap: %q -> %v", answer, err)
		}
	}
	// A recap that only mentions the marker in prose is still a recap.
	runOmp = func(context.Context, claudeSession, string) (string, error) {
		return "Teaching the `|DSML|` guard to leave prose alone. Next, add a test.", nil
	}
	if _, err := runOmpRecap(context.Background(), withDefaults(config{}), claudeSession{ID: ompNative}); err != nil {
		t.Errorf("a recap was refused: %v", err)
	}
}

func TestOnlyStaleOmpForkRunsArePruned(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	stale, inFlight := filepath.Join(forkDir(), "run-old"), filepath.Join(forkDir(), "run-now")
	for _, dir := range []string{stale, inFlight} {
		write(t, filepath.Join(dir, "fork.jsonl"), "{}\n")
	}
	past := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}
	pruneForkRuns()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a day-old run is still there: %v", err)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Errorf("a run that may be in flight was pruned: %v", err)
	}
}

func TestConcurrentOmpRecapsEachWriteTheOverlay(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				if _, err := writeOmpOverlay(); err != nil {
					t.Errorf("an overlay write failed: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if data, err := os.ReadFile(ompOverlayPath()); err != nil || string(data) != ompOverlay {
		t.Errorf("the overlay is %d bytes: %v", len(data), err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(ompOverlayPath()), ".omp-fork-*.yml")); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

func TestOmpRecapsLeaveTheTerminalAndHerdrBehind(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("TMUX_PANE", "%3")
	t.Setenv("WT_SESSION", "abc")
	for _, kv := range ompEnv(claudeSession{}) {
		for _, gone := range []string{"HERDR_", "TMUX_PANE=", "WT_SESSION="} {
			if strings.HasPrefix(kv, gone) {
				t.Errorf("the recap's environment keeps %q", kv)
			}
		}
	}
	// The pane's PATH is the one its omp was found on.
	if env := ompEnv(claudeSession{Path: "/agent/bin"}); !slices.Contains(env, "PATH=/agent/bin") {
		t.Errorf("%v", env)
	}
}

func TestTheOmpForkIsDeletedOnceTheRecapIsTaken(t *testing.T) {
	state := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", state)
	record := filepath.Join(t.TempDir(), "spawned")
	t.Setenv("RECORD", record)
	const forkScript = "#!/bin/sh\n" +
		"while [ $# -gt 0 ]; do case \"$1\" in" +
		" --session-dir) printf '%s' \"$2\" > \"$RECORD\"; shift 2;;" +
		" --config) shift 2;;" +
		" *) shift;; esac; done\n"
	for _, tc := range []struct {
		name  string
		body  string
		fails bool
	}{
		{"recap taken", `echo "goal: x. next: y."`, false},
		{"fork failed", `echo "no model" >&2; exit 3`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "omp")
			write(t, bin, forkScript+tc.body+"\n")
			os.Chmod(bin, 0o755)
			s := claudeSession{Binary: bin, Transcript: "/sessions/x.jsonl", Cwd: t.TempDir()}
			out, err := runOmp(context.Background(), s, "prompt")
			if tc.fails != (err != nil) {
				t.Fatalf("error %v", err)
			}
			if !tc.fails && !strings.Contains(out, "goal: x") {
				t.Errorf("the fork answered %q", out)
			}
			data, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			dir := string(data)
			if !strings.HasPrefix(dir, state) {
				t.Errorf("the fork was written outside the plugin's state dir: %q", dir)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("the fork is still there: %v", err)
			}
		})
	}
	// The overlay the fork is passed lists what a recap isn't to load
	overlay, err := os.ReadFile(ompOverlayPath())
	if err != nil || !strings.Contains(string(overlay), "disabledProviders:") {
		t.Errorf("%v %q", err, overlay)
	}
}
