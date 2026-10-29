package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wouteroostervld/motorsaw/pkg/chunker"
)

const hookUsage = `Usage: motorsaw hook read
       motorsaw hook mode [deny|context|off|default]

A Claude Code PreToolUse hook for the Read tool. When an agent reads a large
source file whole, the hook hands it the file's outline, so that it can read
the part it needs instead of the whole file. Reads with an offset or a limit,
small files and files motorsaw cannot parse are left alone, and so is a
second whole read of the same file in a session: an agent that does need all
of it gets it.

Only files motorsaw can parse are looked at, as their name says, and only
regular files of up to 16 MB that are text: a video, an executable or a
binary under a source name is let through untouched, without being read.

Reads the hook's JSON on standard input and writes its answer on standard
output. It never fails a read: on any error it says nothing.

"motorsaw hook mode" switches the hook for a project from within a session,
for instance from a slash command: it writes the mode to the file
.claude/motorsaw-read-hook in the project, which the hook reads on every Read
and which comes before the environment. "default" removes the file, and with
it what the environment says holds again; with no mode it shows the mode in
force and where it comes from. The project is $CLAUDE_PROJECT_DIR, or else the
nearest directory up from here with a .claude directory. The hook looks for
the file from $CLAUDE_PROJECT_DIR and from the session's directory upwards, so
agents in worktrees under the project are switched too.

Environment:
  MOTORSAW_READ_HOOK        off (the default): do nothing
                            context: let the read go ahead, with the outline
                              added to the context (costs tokens, saves none)
                            deny: refuse the read and give the outline instead;
                              a read with offset and limit, or the same read
                              again, goes through
  MOTORSAW_READ_HOOK_LINES  files with fewer lines are left alone (default 300)
  MOTORSAW_READ_HOOK_MAX    longest outline handed over, in bytes (default 16000)

In settings.json:
  "hooks": {"PreToolUse": [{"matcher": "Read",
    "hooks": [{"type": "command", "command": "motorsaw hook read"}]}]},
  "env": {"MOTORSAW_READ_HOOK": "deny"}`

// hookInput is the part of a PreToolUse hook's input the read hook uses
type hookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		FilePath string          `json:"file_path"`
		Offset   json.RawMessage `json:"offset"`
		Limit    json.RawMessage `json:"limit"`
	} `json:"tool_input"`
}

type hookSpecific struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
}

type hookOutput struct {
	HookSpecificOutput hookSpecific `json:"hookSpecificOutput"`
}

// readHook settings, from the environment
type readHookConfig struct {
	mode     string // off, context or deny
	minLines int
	maxBytes int
	maxFile  int64  // larger files are not looked at
	stateDir string // where the files already outlined in a session are kept
	// project finds the mode a project was switched to, for the session's
	// directory; "" when it was not switched
	project func(cwd string) string
}

// modeFile is where "motorsaw hook mode" keeps a project's mode
const modeFile = ".claude/motorsaw-read-hook"

// readModes are the modes the hook knows
var readModes = map[string]bool{"off": true, "context": true, "deny": true}

// projectMode finds the mode file for a session's directory: in
// $CLAUDE_PROJECT_DIR, or in the directory or one of those above it
func projectMode(cwd string) (mode, file string) {
	var dirs []string
	if p := os.Getenv("CLAUDE_PROJECT_DIR"); p != "" {
		dirs = append(dirs, p)
	}
	for d := cwd; d != "" && filepath.IsAbs(d); d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	for _, d := range dirs {
		f := filepath.Join(d, modeFile)
		if b, err := os.ReadFile(f); err == nil {
			if m := strings.ToLower(strings.TrimSpace(string(b))); readModes[m] {
				return m, f
			}
		}
	}
	return "", ""
}

func readHookConfigFromEnv() readHookConfig {
	number := func(name string, def int) int {
		if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
			return n
		}
		return def
	}
	return readHookConfig{
		mode:     strings.ToLower(strings.TrimSpace(os.Getenv("MOTORSAW_READ_HOOK"))),
		minLines: number("MOTORSAW_READ_HOOK_LINES", 300),
		maxBytes: number("MOTORSAW_READ_HOOK_MAX", 16000),
		maxFile:  16 << 20,
		stateDir: filepath.Join(os.TempDir(), "motorsaw-read-hook"),
		project:  func(cwd string) string { m, _ := projectMode(cwd); return m },
	}
}

func handleHook(args []string) {
	if len(args) >= 1 && args[0] == "mode" && len(args) <= 2 {
		handleHookMode(args[1:])
		return
	}
	if len(args) != 1 || args[0] != "read" {
		fmt.Println(hookUsage)
		os.Exit(1)
	}
	out := readHook(os.Stdin, readHookConfigFromEnv())
	if out != nil {
		json.NewEncoder(os.Stdout).Encode(out)
	}
}

// readHook decides about one Read. nil leaves the read alone.
func readHook(stdin io.Reader, cfg readHookConfig) *hookOutput {
	var in hookInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil || in.ToolName != "Read" {
		return nil
	}
	if cfg.project != nil {
		if m := cfg.project(in.Cwd); m != "" {
			cfg.mode = m
		}
	}
	if cfg.mode != "context" && cfg.mode != "deny" {
		return nil
	}
	if given(in.ToolInput.Offset) || given(in.ToolInput.Limit) {
		return nil
	}
	path := in.ToolInput.FilePath
	// Decided before the file is read: a video or an executable is not a
	// source file, and neither is anything that is not a regular file
	if !chunker.Supported(path) {
		return nil
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() || info.Size() > cfg.maxFile {
		return nil
	}
	content, err := os.ReadFile(path)
	if err != nil || bytes.IndexByte(content[:min(len(content), 8000)], 0) >= 0 {
		// Binary, whatever its name says
		return nil
	}
	lines := bytes.Count(content, []byte("\n"))
	if lines < cfg.minLines {
		return nil
	}
	if cfg.mode == "deny" && !firstWholeRead(cfg.stateDir, in.SessionID, path) {
		return nil
	}
	f, ok := chunker.Parse(path, content, 0)
	if !ok {
		return nil
	}
	outline := f.Render(nil)
	if len(outline) > cfg.maxBytes {
		outline = outline[:cfg.maxBytes] + "\n[outline cut at " + strconv.Itoa(cfg.maxBytes) + " bytes]\n"
	}

	how := readOn(f, path)
	if cfg.mode == "context" {
		return &hookOutput{hookSpecific{
			HookEventName: "PreToolUse",
			AdditionalContext: fmt.Sprintf("%s has %d lines. Its outline, as `motorsaw outline %s` prints it:\n\n%s\n%s",
				path, lines, shellQuote(path), outline, how),
		}}
	}
	return &hookOutput{hookSpecific{
		HookEventName:      "PreToolUse",
		PermissionDecision: "deny",
		PermissionDecisionReason: fmt.Sprintf("Not read whole: %s has %d lines. Its outline, as `motorsaw outline %s` "+
			"prints it:\n\n%s\n%s", path, lines, shellQuote(path), outline, how),
	}}
}

// readOn says how to read on from an outline, with commands that can be run
// as they are: they name the largest definition in the file
func readOn(f *chunker.File, path string) string {
	q := shellQuote(path)
	var b strings.Builder
	b.WriteString("To read on:\n")
	b.WriteString("- some lines: Read with offset and limit; the outline gives the lines of each definition\n")
	largest := -1
	for i, s := range f.Symbols {
		if largest < 0 || s.EndLine-s.StartLine > f.Symbols[largest].EndLine-f.Symbols[largest].StartLine {
			largest = i
		}
	}
	if largest >= 0 {
		s := f.Symbols[largest]
		fmt.Fprintf(&b, "- one definition: `motorsaw outline --body %s %s` (lines %d-%d)\n", s.QualifiedName(), q, s.StartLine, s.EndLine)
		fmt.Fprintf(&b, "- where a name is used, by definition: `motorsaw grep -s '%s' %s`\n", s.Name, shellQuote(filepath.Dir(path)))
	}
	b.WriteString("- all of it: Read the whole file again; a second whole read goes through\n")
	return b.String()
}

// shellQuote quotes a path for a shell when it needs it
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r == '/' || r == '.' || r == '_' || r == '-' || r == '+' || r == ':' || r == '@' ||
			('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9'))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// given reports whether an optional number was passed
func given(v json.RawMessage) bool {
	s := strings.TrimSpace(string(v))
	return s != "" && s != "null"
}

// firstWholeRead records a whole read of a file in a session, and reports
// whether it is the first. When it cannot tell, it says yes once more rather
// than block reads for good.
func firstWholeRead(dir, session, path string) bool {
	if session == "" || strings.ContainsAny(session, `/\`) {
		return true
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return true
	}
	file := filepath.Join(dir, session)
	seen, _ := os.ReadFile(file)
	for _, line := range strings.Split(string(seen), "\n") {
		if line == path {
			return false
		}
	}
	if f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600); err == nil {
		fmt.Fprintln(f, path)
		f.Close()
	}
	return true
}

// handleHookMode shows or sets the mode of the read hook for the project
func handleHookMode(args []string) {
	cwd := workingDir()
	if len(args) == 0 {
		if m, f := projectMode(cwd); m != "" {
			fmt.Printf("read hook: %s, set for the project in %s\n", m, f)
			return
		}
		m := strings.ToLower(strings.TrimSpace(os.Getenv("MOTORSAW_READ_HOOK")))
		if !readModes[m] {
			m = "off"
		}
		fmt.Printf("read hook: %s, from MOTORSAW_READ_HOOK (the project has not switched it)\n", m)
		return
	}

	mode := strings.ToLower(args[0])
	if !readModes[mode] && mode != "default" {
		fail("the mode is deny, context, off or default, not %q", args[0])
	}
	dir := os.Getenv("CLAUDE_PROJECT_DIR")
	if dir == "" {
		for d := cwd; ; d = filepath.Dir(d) {
			if info, err := os.Stat(filepath.Join(d, ".claude")); err == nil && info.IsDir() {
				dir = d
				break
			}
			if filepath.Dir(d) == d {
				dir = cwd
				break
			}
		}
	}
	file := filepath.Join(dir, modeFile)
	if mode == "default" {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			fail("%v", err)
		}
		fmt.Printf("read hook: back to MOTORSAW_READ_HOOK for %s\n", dir)
		return
	}
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		fail("%v", err)
	}
	if err := os.WriteFile(file, []byte(mode+"\n"), 0644); err != nil {
		fail("%v", err)
	}
	fmt.Printf("read hook: %s for %s, from the next Read on (in %s)\n", mode, dir, file)
}
