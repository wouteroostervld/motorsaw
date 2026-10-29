package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBig writes a Go file of about n lines with one function per 10 lines
func writeBig(t *testing.T, n int) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("package big\n\n")
	for i := 0; strings.Count(b.String(), "\n") < n; i++ {
		fmt.Fprintf(&b, "func F%d(x int) int {\n", i)
		for j := 0; j < 7; j++ {
			fmt.Fprintf(&b, "\tx += %d\n", j)
		}
		b.WriteString("\treturn x\n}\n")
	}
	path := filepath.Join(t.TempDir(), "big.go")
	if err := os.WriteFile(path, []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/a/b_c.go":    "/a/b_c.go",
		"/a b/c.go":    "'/a b/c.go'",
		"/it's/x.go":   `'/it'\''s/x.go'`,
		"/a/$HOME.php": "'/a/$HOME.php'",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func hookIn(session, path, extra string) *strings.Reader {
	return strings.NewReader(fmt.Sprintf(`{"session_id":%q,"tool_name":"Read","tool_input":{"file_path":%q%s}}`, session, path, extra))
}

func TestReadHook(t *testing.T) {
	big := writeBig(t, 400)
	small := writeBig(t, 20)
	text := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(text, []byte(strings.Repeat("a line\n", 500)), 0600)
	cfg := func(mode string) readHookConfig {
		return readHookConfig{mode: mode, minLines: 300, maxBytes: 16000, stateDir: t.TempDir()}
	}

	deny := cfg("deny")
	out := readHook(hookIn("s1", big, ""), deny)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("a first whole read of a large file: got %+v, want deny", out)
	}
	reason := out.HookSpecificOutput.PermissionDecisionReason
	if !strings.Contains(reason, "func F0(x int) int") || strings.Contains(reason, "x += 3") {
		t.Errorf("the reason should hold the outline, signatures without bodies:\n%s", reason)
	}
	// ... and commands that can be run as they are, on this file
	for _, want := range []string{"`motorsaw outline " + big + "`", "`motorsaw outline --body F0 " + big + "`",
		"`motorsaw grep -s 'F0' " + filepath.Dir(big) + "`"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the reason should name %s:\n%s", want, reason)
		}
	}
	if got := readHook(hookIn("s1", big, ""), deny); got != nil {
		t.Errorf("the same whole read again must go through, got %+v", got)
	}
	if got := readHook(hookIn("s2", big, ""), deny); got == nil {
		t.Errorf("another session's first read is refused too")
	}

	for name, c := range map[string]struct {
		in  *strings.Reader
		cfg readHookConfig
	}{
		"off":          {hookIn("s3", big, ""), cfg("")},
		"unknown mode": {hookIn("s3", big, ""), cfg("yes")},
		"with offset":  {hookIn("s3", big, `,"offset":10`), deny},
		"with limit":   {hookIn("s3", big, `,"limit":50`), deny},
		"small file":   {hookIn("s3", small, ""), deny},
		"not parsable": {hookIn("s3", text, ""), deny},
		"missing file": {hookIn("s3", "/nonexistent/x.go", ""), deny},
		"other tool":   {strings.NewReader(`{"tool_name":"Edit","tool_input":{"file_path":"x"}}`), deny},
		"not json":     {strings.NewReader(`{`), deny},
	} {
		if got := readHook(c.in, c.cfg); got != nil {
			t.Errorf("%s: got %+v, want the read left alone", name, got)
		}
	}

	ctx := readHook(hookIn("s4", big, ""), cfg("context"))
	if ctx == nil || ctx.HookSpecificOutput.PermissionDecision != "" || !strings.Contains(ctx.HookSpecificOutput.AdditionalContext, "func F0(x int) int") ||
		!strings.Contains(ctx.HookSpecificOutput.AdditionalContext, "motorsaw outline --body") {
		t.Errorf("context mode: got %+v, want the read allowed with the outline added", ctx)
	}

	short := cfg("deny")
	short.maxBytes = 100
	if out := readHook(hookIn("s5", big, ""), short); out == nil || !strings.Contains(out.HookSpecificOutput.PermissionDecisionReason, "[outline cut at 100 bytes]") {
		t.Errorf("a long outline is cut: got %+v", out)
	}
}

func TestReadHookProjectMode(t *testing.T) {
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	big := writeBig(t, 400)
	project := t.TempDir()
	nested := filepath.Join(project, ".claude", "worktrees", "agent", "src")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	set := func(mode string) {
		if err := os.WriteFile(filepath.Join(project, modeFile), []byte(mode+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	in := func(session, cwd string) *strings.Reader {
		return strings.NewReader(fmt.Sprintf(`{"session_id":%q,"cwd":%q,"tool_name":"Read","tool_input":{"file_path":%q}}`, session, cwd, big))
	}
	cfg := func(env string) readHookConfig {
		return readHookConfig{mode: env, minLines: 300, maxBytes: 16000, stateDir: t.TempDir(),
			project: func(cwd string) string { m, _ := projectMode(cwd); return m }}
	}

	// Not switched: the environment decides
	if readHook(in("a", project), cfg("")) != nil {
		t.Error("no file and no environment: the read must be left alone")
	}
	// The project switched it on, against the environment, also for an agent in a worktree below it
	set("deny")
	if out := readHook(in("b", nested), cfg("off")); out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Errorf("the project's deny must win over MOTORSAW_READ_HOOK=off, got %+v", out)
	}
	// ... and off, against the environment
	set("off")
	if out := readHook(in("c", project), cfg("deny")); out != nil {
		t.Errorf("the project's off must win over MOTORSAW_READ_HOOK=deny, got %+v", out)
	}
	// A file that says nothing the hook knows is no switch
	set("maybe")
	if out := readHook(in("d", project), cfg("deny")); out == nil {
		t.Error("an unknown mode in the file must leave the environment in force")
	}
	// $CLAUDE_PROJECT_DIR is looked in even when the session is elsewhere
	set("off")
	t.Setenv("CLAUDE_PROJECT_DIR", project)
	if m, f := projectMode(t.TempDir()); m != "off" || f != filepath.Join(project, modeFile) {
		t.Errorf("projectMode with CLAUDE_PROJECT_DIR = %q, %q", m, f)
	}
}
