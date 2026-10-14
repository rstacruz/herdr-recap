package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// omp recaps itself, as Claude and Codex do: a fork of the pane's conversation
// answers one prompt. Those forks always land on disk, so a recap runs in a
// directory of the plugin's own, removed whole once it is taken

// ompSessionID is the session a session file is for, named
// <timestamp>_<id>.jsonl as omp writes it; "" for any other file
func ompSessionID(path string) string {
	if filepath.Ext(path) != ".jsonl" {
		return ""
	}
	name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if i := strings.LastIndex(name, "_"); i >= 0 && sessionIDPattern.MatchString(name[i+1:]) {
		return name[i+1:]
	}
	return ""
}

// ompTop is what a session file's top says. omp writes the file before a pane
// has said anything, so Said false is a pane to come back to, not a
// conversation to recap
type ompTop struct {
	ID   string
	Cwd  string
	Said bool
}

// ompTopLines is how far into a session file its record and first message are
// looked for, since a long-running session's file is big
const ompTopLines = 64

// readOmpTop reads the top of a session file; one that isn't there yet wraps
// os.ErrNotExist, since omp writes it with a session's first turn
func readOmpTop(path string) (ompTop, error) {
	f, err := os.Open(path)
	if err != nil {
		return ompTop{}, err
	}
	defer f.Close()
	var top ompTop
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for n := 0; sc.Scan() && n < ompTopLines; n++ {
		var rec struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Cwd     string `json:"cwd"`
			Message *struct {
				Role string `json:"role"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		switch rec.Type {
		case "session":
			if memexSessionIDPattern.MatchString(rec.ID) {
				top.ID = rec.ID
			}
			top.Cwd = rec.Cwd
		case "message":
			if rec.Message != nil && (rec.Message.Role == "user" || rec.Message.Role == "assistant") {
				top.Said = true
				return top, nil
			}
		}
	}
	switch err := sc.Err(); {
	case errors.Is(err, bufio.ErrTooLong):
		// A line too long to read is some message's: something was said.
		top.Said = true
	case err != nil:
		return top, err
	}
	return top, nil
}

// resolveOmpSession finds the omp session from the path herdr reports for it:
// its transcript, the id from its file name or record, the cwd it started in.
// Nothing said yet means no transcript to fork
func resolveOmpSession(a agentInfo) (claudeSession, error) {
	if a.AgentSession.Kind != "path" || a.AgentSession.Value == "" {
		return claudeSession{}, errors.New("herdr has no session path for it")
	}
	path := a.AgentSession.Value
	s := claudeSession{Agent: "omp", Native: true, ID: ompSessionID(path), Cwd: a.Cwd}
	// The recap runs as the pane's omp does: its PATH and binary, since
	// herdr's own PATH often has neither
	if procs, err := paneProcs(a.PaneID); err == nil && len(procs) > 0 {
		pid, argv0 := binaryPID(procs, "omp")
		if pid == 0 {
			pid = procs[len(procs)-1].PID
		}
		env, _ := processEnv(pid)
		s.Path, s.Binary = env["PATH"], agentBinary(argv0, env)
	}
	top, err := readOmpTop(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Nothing said yet, and nowhere yet to have said it.
	case err != nil:
		return claudeSession{}, fmt.Errorf("omp's session file: %w", err)
	default:
		if top.ID != "" {
			s.ID = top.ID
		}
		if top.Cwd != "" {
			s.Cwd = top.Cwd
		}
		if top.Said {
			s.Transcript = path
		}
	}
	if s.ID == "" {
		return claudeSession{}, errors.New("herdr's session path isn't an omp session file")
	}
	return s, nil
}

// ompOverlay is the config a recap's fork runs with, passed as --config: omp
// has no --no-mcp, so what the recap must not load is disabled here. Each entry
// is a discovery provider that can contribute MCP servers or tools (omp
// 18.10.1's `mcps` takes twelve; codex's, gemini's and opencode's project
// configs load whether or not their own are opted into)
const ompOverlay = `# herdr-recap: what its omp recaps run with. A settings layer (--config),
# so its array replaces any disabledProviders you set yourself.
disabledProviders:
  - native
  - mcp-json
  - agents
  - agent-plugins
  - claude
  - claude-plugins
  - codex
  - cursor
  - gemini
  - omp-plugins
  - opencode
  - vscode
  - windsurf
`

// ompOverlayPath is the overlay's path in the plugin's state dir
func ompOverlayPath() string { return filepath.Join(stateDir(), "omp-fork.yml") }

// forkDir is where a recap's fork runs are written, one directory each
func forkDir() string { return filepath.Join(stateDir(), "fork") }

// pruneForkRuns removes fork run directories whose recap is long over: one
// killed mid-run (a restart mid-recap, a SIGKILL) leaves its fork and the
// session's copied artifacts behind
func pruneForkRuns() {
	entries, _ := os.ReadDir(forkDir())
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			os.RemoveAll(filepath.Join(forkDir(), e.Name()))
		}
	}
}

// writeOmpOverlay writes the overlay out for a fork to be passed it, through a
// temporary file of its own and a rename: a fork starting mid-write reads the
// old or the new file, never a truncated one. A name per write, since several
// recaps run at once (the popup's rows, each pane's tick): shared, one recap's
// rename strands another
func writeOmpOverlay() (string, error) {
	path := ompOverlayPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".omp-fork-*.yml")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name()) // no-op once the rename has taken it
	if _, err := f.WriteString(ompOverlay); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// ompPrompt asks for a recap like omp's own (recap-user.md): under 40 words,
// the goal and current task, then one next action. It opens by stopping the
// work, since a fork of a mid-turn pane ends on a step omp has no result for
// and the model can carry it on with a tool call instead of recapping (seen
// 2026-10-14 on omp 18.10.1; a system prompt framing changed nothing and cost
// the fork its prompt cache). runOmpRecap refuses a tool call
const ompPrompt = "Stop: this is a recap request, not a continuation of the work above, and any step it ends on is not to be carried out. " +
	"Recap this session in under 40 words, for someone coming back to it: the overall goal and the current task, then one next action. " +
	"Don't use any tools. Answer with 1-2 plain sentences, no markdown, and the recap alone."

// ompToolCall is omp's tool-call marker, as a model writing one spells it: an
// opening or closing "DSML" tag with one full-width bar (omp 18.10.1's own
// renderer and parser; ASCII `|` too) or a doubled one (seen 2026-10-14). The
// `<` is required: a recap that mentions the marker in prose is still a recap
var ompToolCall = regexp.MustCompile(`<\/?[｜|]{1,2}DSML`)

// ompArgs forks the session for one answer to prompt, in a run that writes
// nothing of the user's or the project's: a session dir of the plugin's own,
// the bare flags
func ompArgs(session, dir, overlay, prompt string) []string {
	return []string{"--fork", session, "--session-dir", dir, "--config", overlay,
		"--no-tools", "--no-extensions", "--no-skills", "--no-rules", "--no-title", "--thinking", "off",
		"-p", prompt}
}

// terminalVars are how omp identifies its terminal when stdin isn't one, as a
// recap's isn't: getTerminalId's fallback list (packages/tui/src/ttyid.ts, omp
// 18.10.1). Left in, the fork reports to the pane's terminal and leaves it a
// --continue breadcrumb to a session about to be deleted
var terminalVars = []string{
	"ZELLIJ_PANE_ID", "ZELLIJ_SESSION_NAME", "TMUX_PANE", "CMUX_SURFACE_ID",
	"KITTY_WINDOW_ID", "WEZTERM_PANE", "TERM_SESSION_ID", "WT_SESSION",
}

// ompEnv is the environment a recap runs with: the plugin's, less herdr's and
// the terminal's, with the agent's PATH
func ompEnv(s claudeSession) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "HERDR_") || slices.Contains(terminalVars, name) || (s.Path != "" && name == "PATH") {
			continue
		}
		env = append(env, kv)
	}
	if s.Path != "" {
		env = append(env, "PATH="+s.Path)
	}
	return env
}

// ompBinary is the omp a recap runs: the pane's own, else "omp" on PATH
func ompBinary(s claudeSession) string {
	if s.Binary != "" {
		return s.Binary
	}
	return "omp"
}

// runOmp runs the fork and returns what it answered; a variable so tests can
// stand one in.
var runOmp = func(ctx context.Context, s claudeSession, prompt string) (string, error) {
	overlay, err := writeOmpOverlay()
	if err != nil {
		return "", err
	}
	fork := forkDir()
	if err := os.MkdirAll(fork, 0o700); err != nil {
		return "", err
	}
	// A directory per run: the fork's file and the session's copied
	// artifacts, removed whole when the recap is taken, on success or failure
	dir, err := os.MkdirTemp(fork, "run-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(ctx, ompBinary(s), ompArgs(s.Transcript, dir, overlay, prompt)...)
	// The session's own directory: the fork belongs to the project it was
	// had in
	cmd.Dir, cmd.Env = s.Cwd, ompEnv(s)
	cmd.WaitDelay = 5 * time.Second
	out, stderr, err := runBoundedCommand(cmd)
	if err != nil {
		if msg := strings.TrimSpace(stderr); msg != "" && !errors.Is(err, errOutputLimit) {
			return "", fmt.Errorf("%w: %s", err, shorten(clean(lastLine(msg), false), 200))
		}
		return "", err
	}
	return string(out), nil
}

// runOmpRecap recaps an omp session from a fork of it, as runCodexRecap does
// a Codex session's.
func runOmpRecap(ctx context.Context, cfg config, s claudeSession) (recap, error) {
	out, err := runOmp(ctx, s, ompPrompt)
	if err != nil {
		return recap{}, fmt.Errorf("omp: %w", err)
	}
	text := strings.Join(strings.Fields(clean(out, false)), " ")
	if text == "" {
		return recap{}, errors.New("omp gave an empty recap")
	}
	if ompToolCall.MatchString(text) {
		// The fork carried the work on rather than recapping it: a next step
		// shown as the recap is worse than no recap
		return recap{}, errors.New("omp answered with a tool call, not a recap")
	}
	return recap{Session: s.ID, Source: "omp", Text: text, At: time.Now()}, nil
}
