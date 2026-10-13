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
	"slices"
	"strings"
	"time"
)

// omp recaps itself, as Claude and Codex do: `omp --fork <session> -p <prompt>`
// answers one prompt in a fork of the pane's conversation, writing nothing
// back to it. Unlike theirs, omp's fork always lands on disk (it has none that
// doesn't), so it's written into a directory of the plugin's own, which is
// removed whole once the recap is taken.

// ompSessionID is the session a session file is for, from its name:
// <timestamp>_<id>.jsonl, as omp writes it under sessions/. "" for any other
// file.
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

// ompTop is what the top of a session file says: the session's own record
// (its id and the directory it was started in) and whether anything of the
// conversation is there yet. A pane's session file can be written before
// anything is said in it (a title slot, a header and its bookkeeping), so one
// with no message yet is a pane to come back to, not a conversation to recap.
type ompTop struct {
	ID   string
	Cwd  string
	Said bool
}

// ompTopLines is as far into a session file its record and first message are
// looked for: both are at the top, and a long-running session's file is big.
const ompTopLines = 64

// readOmpTop reads the top of a session file. A file that isn't there yet
// comes back as an error wrapping os.ErrNotExist: omp writes a session's file
// with its first turn, so a pane that hasn't had one has none.
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

// resolveOmpSession finds the omp session in a pane from the path herdr
// reports for it: the transcript, the id from its file name or its own
// record, and the cwd the session was started in. A session with nothing
// said yet comes back without a transcript, as a Claude one does.
func resolveOmpSession(a agentInfo) (claudeSession, error) {
	if a.AgentSession.Kind != "path" || a.AgentSession.Value == "" {
		return claudeSession{}, errors.New("herdr has no session path for it")
	}
	path := a.AgentSession.Value
	s := claudeSession{Agent: "omp", Native: true, ID: ompSessionID(path), Cwd: a.Cwd}
	// The recap runs as the pane's omp does: its PATH, and the binary it was
	// started as, since herdr's own PATH often hasn't either.
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

// ompOverlay is the config an omp recap's fork runs with, written out as
// omp-fork.yml and passed as --config: omp has no --no-mcp, so what a recap
// has no business loading is taken out here instead. It's a settings layer,
// so its array replaces any disabledProviders the user set: the fork loads
// none of them.
const ompOverlay = `# herdr-recap: what its omp recaps run with. A settings layer (--config),
# so its array replaces any disabledProviders you set yourself.
disabledProviders:
  - native
  - mcp-json
  - agents
  - claude
  - claude-plugins
  - omp-plugins
  - agent-plugins
  - cursor
  - windsurf
  - vscode
`

// ompOverlayPath is where the overlay lives, in the plugin's state dir.
func ompOverlayPath() string { return filepath.Join(stateDir(), "omp-fork.yml") }

// writeOmpOverlay writes the overlay out, for a fork to be passed it.
func writeOmpOverlay() (string, error) {
	path := ompOverlayPath()
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(ompOverlay), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// ompPrompt asks for what omp's own recap of an idle session says
// (recap-user.md): under 40 words, the goal and the current task, then one
// next action.
const ompPrompt = "Recap this session in under 40 words, for someone coming back to it: the overall goal and the current task, " +
	"then one next action. Answer with 1-2 plain sentences, no markdown, and the recap alone."

// ompArgs forks the session for one answer to prompt, in a run the fork can
// write nothing of the project's or the user's into: a session dir of the
// plugin's own, the bare flags, and the overlay's config.
func ompArgs(session, dir, overlay, prompt string) []string {
	return []string{"--fork", session, "--session-dir", dir, "--config", overlay,
		"--no-tools", "--no-extensions", "--no-skills", "--no-rules", "--no-title", "--thinking", "off",
		"-p", prompt}
}

// terminalVars are the variables omp identifies the terminal it runs in by
// when stdin isn't one of them, as a fork run by a hook or the popup isn't.
// Kept out of a recap's environment, the fork reports to nothing and leaves
// no --continue breadcrumb for the pane, pointing at a session the plugin is
// about to delete.
var terminalVars = []string{
	"ZELLIJ_PANE_ID", "ZELLIJ_SESSION_NAME", "TMUX_PANE", "CMUX_SURFACE_ID",
	"KITTY_WINDOW_ID", "WEZTERM_PANE", "TERM_SESSION_ID", "WT_SESSION",
}

// ompEnv is the environment an omp recap runs with: the plugin's, less
// anything that would tie it to herdr (as a Claude recap's is), less the
// terminal's own identity, with the agent's PATH.
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

// ompBinary is the omp a recap runs: the one the pane's agent runs, else
// whatever "omp" is on PATH.
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
	forkDir := filepath.Join(stateDir(), "fork")
	if err := os.MkdirAll(forkDir, 0o700); err != nil {
		return "", err
	}
	// A directory per run: it holds the fork's file and, beside it, what the
	// session's artifacts were copied to, and goes whole when the recap is
	// taken, on success or failure.
	dir, err := os.MkdirTemp(forkDir, "run-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(ctx, ompBinary(s), ompArgs(s.Transcript, dir, overlay, prompt)...)
	// In the session's own directory: the fork is of that conversation, in
	// the project it was had in.
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
	return recap{Session: s.ID, Source: "omp", Text: text, At: time.Now()}, nil
}
