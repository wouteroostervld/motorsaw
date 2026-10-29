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

A Claude Code PreToolUse hook for the Read tool. When an agent reads a large
source file whole, the hook hands it the file's outline, so that it can read
the part it needs instead of the whole file. Reads with an offset or a limit,
small files and files motorsaw cannot parse are left alone, and so is a
second whole read of the same file in a session: an agent that does need all
of it gets it.

Reads the hook's JSON on standard input and writes its answer on standard
output. It never fails a read: on any error it says nothing.

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
	stateDir string // where the files already outlined in a session are kept
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
		stateDir: filepath.Join(os.TempDir(), "motorsaw-read-hook"),
	}
}

func handleHook(args []string) {
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
	if cfg.mode != "context" && cfg.mode != "deny" {
		return nil
	}
	var in hookInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil || in.ToolName != "Read" {
		return nil
	}
	if given(in.ToolInput.Offset) || given(in.ToolInput.Limit) {
		return nil
	}
	path := in.ToolInput.FilePath
	content, err := os.ReadFile(path)
	if err != nil {
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
