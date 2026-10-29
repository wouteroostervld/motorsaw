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
	if ctx == nil || ctx.HookSpecificOutput.PermissionDecision != "" || !strings.Contains(ctx.HookSpecificOutput.AdditionalContext, "func F0(x int) int") {
		t.Errorf("context mode: got %+v, want the read allowed with the outline added", ctx)
	}

	short := cfg("deny")
	short.maxBytes = 100
	if out := readHook(hookIn("s5", big, ""), short); out == nil || !strings.Contains(out.HookSpecificOutput.PermissionDecisionReason, "[outline cut at 100 bytes]") {
		t.Errorf("a long outline is cut: got %+v", out)
	}
}
