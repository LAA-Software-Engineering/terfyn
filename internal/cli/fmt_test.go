package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFmt_checkDetectsChange(t *testing.T) {
	root := testdataPath(t, "fmt_messy")

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"fmt", "--check", "--project", root})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected check failure")
	}
	if ExitCodeOf(err) != ExitGenericFailure {
		t.Fatalf("code=%d err=%v", ExitCodeOf(err), err)
	}
}

func TestFmt_writeThenCheckClean(t *testing.T) {
	srcRoot := testdataPath(t, "fmt_messy")
	root := t.TempDir()
	entries, err := os.ReadDir(srcRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(srcRoot, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"fmt", "--project", root})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	ResetGlobalsForTest()
	cmd = NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"fmt", "--check", "--project", root})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("second check: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(root, "policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "    name:") {
		t.Fatalf("expected 2-space indent, got:\n%s", b)
	}
}

func TestFmt_formatsAgentSources(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "project.yaml"),
		[]byte("apiVersion: agentic.dev/v0\nkind: Project\nmetadata:\n  name: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Deliberately messy indentation and spacing.
	messy := "workflow W(input: X)   {\n" +
		"if input.a&&input.b {\n" +
		"github.get_pr(input.repo)\n" +
		"}\n" +
		"}\n"
	agentPath := filepath.Join(root, "flow.agent")
	if err := os.WriteFile(agentPath, []byte(messy), 0o644); err != nil {
		t.Fatal(err)
	}

	// First format writes canonical form.
	ResetGlobalsForTest()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"fmt", "--project", root})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("fmt: %v", err)
	}

	got, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "    if input.a && input.b {") {
		t.Fatalf("expected canonical .agent formatting, got:\n%s", got)
	}

	// Second run with --check must be clean (idempotent).
	ResetGlobalsForTest()
	cmd = NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"fmt", "--check", "--project", root})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected formatted .agent to pass --check, got: %v", err)
	}
}

// TestFmt_preservesComments is the regression for issue #509: fmt must not delete // comments. Both
// standalone (own-line) and trailing (inline) comments survive a format, and re-running --check is
// clean (idempotent).
func TestFmt_preservesComments(t *testing.T) {
	root := t.TempDir()
	src := "// entry workflow\nworkflow hello(input: any) {\n    return input // dispatch\n}\n"
	agentPath := filepath.Join(root, "main.agent")
	if err := os.WriteFile(agentPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"fmt", "--project", root})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("fmt: %v", err)
	}

	got, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	if !strings.Contains(out, "// entry workflow") {
		t.Fatalf("standalone comment deleted:\n%s", out)
	}
	if !strings.Contains(out, "return input // dispatch") {
		t.Fatalf("trailing comment deleted:\n%s", out)
	}

	// Idempotent: the formatted file passes --check.
	ResetGlobalsForTest()
	cmd = NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"fmt", "--check", "--project", root})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("formatted file with comments must pass --check, got: %v", err)
	}
}

// TestFmt_agentOnlyProjectFormats is the regression for an .agent-only project (no project.yaml —
// the normal shape under ADR 007): fmt must format the .agent sources, not fail with
// "no project.yaml or project.yml".
func TestFmt_agentOnlyProjectFormats(t *testing.T) {
	root := t.TempDir()
	agentPath := filepath.Join(root, "main.agent")
	messy := "workflow hello(input:any){return input}\n"
	if err := os.WriteFile(agentPath, []byte(messy), 0o644); err != nil {
		t.Fatal(err)
	}

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"fmt", "--project", root})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("fmt on an .agent-only project must succeed, got: %v", err)
	}

	got, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "workflow hello(input: any) {") {
		t.Fatalf("expected canonical .agent formatting, got:\n%s", got)
	}

	// Idempotent: a second run with --check is clean.
	ResetGlobalsForTest()
	cmd = NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"fmt", "--check", "--project", root})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("formatted .agent-only project must pass --check, got: %v", err)
	}
}

// TestFmt_emptyProjectErrors proves a directory with neither .agent sources nor a project.yaml
// (almost always a mistyped --project) fails loudly rather than reporting a 0-file success.
func TestFmt_emptyProjectErrors(t *testing.T) {
	root := t.TempDir()
	ResetGlobalsForTest()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"fmt", "--project", root})
	if err := cmd.Execute(); err == nil {
		t.Fatal("fmt on a directory with no .agent sources and no project.yaml must error")
	}
}

func TestFmt_caseFoldedAgentExtensionFormatsAsAgent(t *testing.T) {
	// Discovery matches .agent case-insensitively; the formatter must use the
	// same predicate, or a well-formed .AGENT file would be mangled as YAML.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "project.yaml"),
		[]byte("apiVersion: agentic.dev/v0\nkind: Project\nmetadata:\n  name: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(root, "flow.AGENT")
	if err := os.WriteFile(agentPath, []byte("workflow W(input: X)   {\ngithub.get_pr(input.repo)\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"fmt", "--project", root})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("fmt of a .AGENT file: %v", err)
	}
	got, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	// Canonical .agent form (printer), not a YAML round-trip.
	if !strings.Contains(string(got), "workflow W(input: X) {") {
		t.Fatalf("expected .AGENT to be formatted as .agent, got:\n%s", got)
	}
}

func TestFmt_malformedAgentFails(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "project.yaml"),
		[]byte("apiVersion: agentic.dev/v0\nkind: Project\nmetadata:\n  name: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bad.agent"), []byte("workflow W( {"), 0o644); err != nil {
		t.Fatal(err)
	}
	ResetGlobalsForTest()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"fmt", "--project", root})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected fmt to fail on a malformed .agent source")
	}
	if ExitCodeOf(err) != ExitValidationError {
		t.Fatalf("expected exit %d, got %d (%v)", ExitValidationError, ExitCodeOf(err), err)
	}
}

func TestFmt_secondRunNoop(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"project.yaml", "tool.yaml", "policy.yaml"} {
		src, err := os.ReadFile(filepath.Join(testdataPath(t, "fmt_messy"), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	var out1 bytes.Buffer
	cmd.SetOut(&out1)
	cmd.SetErr(&out1)
	cmd.SetArgs([]string{"fmt", "--project", root})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	ResetGlobalsForTest()
	cmd = NewRootCmd()
	var out2 bytes.Buffer
	cmd.SetOut(&out2)
	cmd.SetErr(&out2)
	cmd.SetArgs([]string{"fmt", "--project", root})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2.String(), "0 unchanged") && !strings.Contains(out2.String(), "3 unchanged") {
		t.Fatalf("expected noop summary, got:\n%s", out2.String())
	}
}

func TestFmt_jsonOutput(t *testing.T) {
	root := testdataPath(t, "fmt_messy")
	ResetGlobalsForTest()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"fmt", "--check", "-o", "json", "--project", root})
	_ = cmd.Execute()
	if !strings.Contains(out.String(), `"changed"`) {
		t.Fatalf("%s", out.String())
	}
}
