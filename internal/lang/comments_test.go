package lang

import (
	"strings"
	"testing"
)

// The issue #509 repro: every comment survives fmt, leading comments stay glued to what they
// document, and trailing inline comments stay on their line.
func TestFormat_preservesComments(t *testing.T) {
	src := `// A greeter agent. This comment documents intent.
agent greeter {
    model mock/default          // inline: the built-in mock model
    instructions """
    Say hello.
    """
}

// The default policy — conservative starting point.
policy default {
    preset shell_safe
}

// Entry workflow.
workflow hello(input: string) -> string
    policy default
{
    return greeter(input)   // dispatch to the agent
}
`
	out, diags := Format("main.agent", src)
	if len(diags) > 0 {
		t.Fatalf("diags: %s", diags.Error())
	}
	// No comment is dropped.
	if got, want := strings.Count(out, "//"), strings.Count(src, "//"); got != want {
		t.Fatalf("comment count: got %d want %d\n%s", got, want, out)
	}
	wantContains := []string{
		"// A greeter agent. This comment documents intent.\nagent greeter {",
		"model mock/default // inline: the built-in mock model",
		"// The default policy — conservative starting point.\npolicy default {",
		"// Entry workflow.\nworkflow hello",
		"return greeter(input) // dispatch to the agent",
	}
	for _, w := range wantContains {
		if !strings.Contains(out, w) {
			t.Fatalf("output missing %q\n--- got ---\n%s", w, out)
		}
	}
}

// Formatting is idempotent even with comments: parse -> print -> parse -> print is stable.
func TestFormat_commentsIdempotent(t *testing.T) {
	src := `// header
tool workspace {
    type native
    operations {
        // read is safe
        read_file { effects { workspace.read } } // trailing
        write_file { effects { workspace.write } }
    }
}

// policy doc
policy p {
    execution {
        maxTotalCostUsd 5
    }
    effects {
        // read only
        permit { workspace.read }
    }
}
`
	once, d1 := Format("t.agent", src)
	if len(d1) > 0 {
		t.Fatalf("diags: %s", d1.Error())
	}
	twice, d2 := Format("t.agent", once)
	if len(d2) > 0 {
		t.Fatalf("reparse diags: %s", d2.Error())
	}
	if once != twice {
		t.Fatalf("not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	if strings.Count(once, "//") != strings.Count(src, "//") {
		t.Fatalf("comment count changed: %d -> %d", strings.Count(src, "//"), strings.Count(once, "//"))
	}
}

// A blank comment (`//` with no text) and a comment after the last declaration both survive.
func TestFormat_blankAndTrailingFileComments(t *testing.T) {
	src := "// one\n//\n// three\nagent a {\n    model m/x\n}\n\n// footer after last decl\n"
	out, diags := Format("a.agent", src)
	if len(diags) > 0 {
		t.Fatalf("diags: %s", diags.Error())
	}
	if !strings.Contains(out, "//\n") {
		t.Fatalf("blank comment line dropped:\n%s", out)
	}
	if !strings.Contains(out, "// footer after last decl") {
		t.Fatalf("footer comment after last decl dropped:\n%s", out)
	}
}

// The lexer classifies own-line comments as standalone and end-of-line comments as trailing.
func TestLexer_commentClassification(t *testing.T) {
	f, diags := Parse("c.agent", "// standalone\nagent a {\n    model m/x // trailing\n}\n")
	if len(diags) > 0 {
		t.Fatalf("diags: %s", diags.Error())
	}
	if len(f.Comments) != 2 {
		t.Fatalf("got %d comments, want 2", len(f.Comments))
	}
	if !f.Comments[0].Standalone || f.Comments[0].Text != "standalone" {
		t.Fatalf("comment 0 = %+v", f.Comments[0])
	}
	if f.Comments[1].Standalone || f.Comments[1].Text != "trailing" {
		t.Fatalf("comment 1 = %+v", f.Comments[1])
	}
}

// Regression for the PR #516 review: comment attachment must follow SOURCE structure, not canonical
// print order. A comment inside a block must not leak to the next declaration, a doc comment above a
// field that canonical order prints later must stay with that field, and trailing comments on
// leaf/inline-block lines must stay attached — none may end up dumped after the enclosing block.
func TestFormat_attachmentFollowsSourceNotPrintOrder(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string // a substring the output MUST contain
		deny string // a substring the output must NOT contain ("" to skip)
	}{
		{
			name: "interior comment does not leak to next decl",
			src:  "tool t {\n    type native\n    // safety note\n    safety { trusted true }\n}\npolicy p {\n    preset shell_safe\n}\n",
			want: "// safety note\n    safety {",
			deny: "// safety note\npolicy p",
		},
		{
			name: "doc comment stays with the field canonical order prints later",
			// grants is before model in source; canonical order prints model first. The comment must
			// stay above grants, not jump onto model.
			src:  "agent a {\n    // grants doc\n    grants { tool.x.y }\n    model mock/x\n}\n",
			want: "// grants doc\n    grants {",
			deny: "// grants doc\n    model",
		},
		{
			name: "trailing comment on description stays inline",
			src:  "agent a {\n    model m/x\n    description \"d\" // why\n}\n",
			want: "description \"d\" // why",
			deny: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, diags := Format("m.agent", tc.src)
			if len(diags) > 0 {
				t.Fatalf("diags: %s", diags.Error())
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("missing %q in:\n%s", tc.want, out)
			}
			if tc.deny != "" && strings.Contains(out, tc.deny) {
				t.Fatalf("unexpected %q in:\n%s", tc.deny, out)
			}
			// Idempotent.
			if out2, _ := Format("m.agent", out); out2 != out {
				t.Fatalf("not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", out, out2)
			}
		})
	}
}

// Regression for the PR #516 second review: a trailing comment on an inner SCALAR line (a
// pointer-typed field with no source position — constraints/safety/execution) has no leaf hook to
// emit it inline, so it must be drained at its enclosing block's tail (as an own-line comment) and
// NEVER leak past the resource to a file-scope dump after the next declaration.
func TestFormat_innerTrailingDoesNotLeakPastResource(t *testing.T) {
	cases := map[string]string{
		"constraints": "agent a {\n    model m/x\n    constraints {\n        maxTokens 32000 // raise\n    }\n}\npolicy p {\n    preset shell_safe\n}\n",
		"safety":      "tool t {\n    safety {\n        trusted true // note\n    }\n}\npolicy p {\n    preset shell_safe\n}\n",
		"execution":   "policy p {\n    execution {\n        maxTotalCostUsd 5 // budget\n    }\n    preset shell_safe\n}\nagent a {\n    model m/x\n}\n",
		"env-overlay": "environment e {\n    overrides {\n        agents {\n            a {\n                constraints {\n                    maxTokens 32000 // raise\n                }\n            }\n        }\n    }\n}\nagent a {\n    model m/x\n}\n",
	}
	needle := map[string]string{"constraints": "raise", "safety": "note", "execution": "budget", "env-overlay": "raise"}
	nextDecl := map[string]string{"constraints": "policy p", "safety": "policy p", "execution": "agent a", "env-overlay": "agent a"}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			out, diags := Format("m.agent", src)
			if len(diags) > 0 {
				t.Fatalf("diags: %s", diags.Error())
			}
			ci := strings.Index(out, "// "+needle[name])
			if ci < 0 {
				t.Fatalf("comment %q dropped:\n%s", needle[name], out)
			}
			// The comment must appear BEFORE the next top-level declaration, i.e. inside its resource.
			if di := strings.Index(out, nextDecl[name]); di >= 0 && ci > di {
				t.Fatalf("trailing comment leaked past %q:\n%s", nextDecl[name], out)
			}
			if out2, _ := Format("m.agent", out); out2 != out {
				t.Fatalf("not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", out, out2)
			}
		})
	}
}

// Regression for the PR #516 third/fourth review: standalone comments inside environment overlay
// wrappers (overrides/agents/<agent>/model) and inside hitl must stay inside their resource — the
// byBlock backstop drains any comment a precise hook missed, so none leaks to file scope.
func TestFormat_overlayAndHitlStandaloneStayInResource(t *testing.T) {
	cases := map[string]struct{ src, needle, next string }{
		"overlay-model": {
			src:    "environment e {\n    overrides {\n        agents {\n            a {\n                // use mock\n                model mock/x\n            }\n        }\n    }\n}\nagent a {\n    model m/x\n}\n",
			needle: "use mock", next: "agent a",
		},
		"overlay-agent-name": {
			src:    "environment e {\n    overrides {\n        agents {\n            // override a\n            a {\n                model mock/x\n            }\n        }\n    }\n}\nagent a {\n    model m/x\n}\n",
			needle: "override a", next: "agent a",
		},
		"hitl": {
			src:    "policy p {\n    hitl {\n        // prefix note\n        descriptionPrefix \"x\"\n    }\n    preset shell_safe\n}\nagent a {\n    model m/x\n}\n",
			needle: "prefix note", next: "agent a",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out, diags := Format("m.agent", tc.src)
			if len(diags) > 0 {
				t.Fatalf("diags: %s", diags.Error())
			}
			ci := strings.Index(out, "// "+tc.needle)
			if ci < 0 {
				t.Fatalf("comment %q dropped:\n%s", tc.needle, out)
			}
			if di := strings.Index(out, tc.next); di >= 0 && ci > di {
				t.Fatalf("comment leaked past %q:\n%s", tc.next, out)
			}
			if out2, _ := Format("m.agent", out); out2 != out {
				t.Fatalf("not idempotent:\n%s", out)
			}
		})
	}
}

// Regression for the PR #516 fifth review: byBlock registers each comment under EVERY enclosing block,
// so a comment inside a block whose printer never calls blockTail (headers, requiredFor) is still
// drained by an ancestor that does — it can never reach file scope. Leak-proof by construction: every
// top-level declaration calls blockTail.
func TestFormat_deepBlockWithoutTailStillContained(t *testing.T) {
	cases := map[string]struct{ src, needle, next string }{
		"headers": {
			src:    "tool t {\n    type mcp\n    mcp {\n        transport \"stdio\"\n        headers {\n            // auth\n            \"Authorization\" \"Bearer x\"\n        }\n    }\n}\nagent a {\n    model m/x\n}\n",
			needle: "auth", next: "agent a",
		},
		"requiredFor": {
			src:    "policy p {\n    approvals {\n        requiredFor {\n            // gate\n            tool.x.y\n        }\n    }\n    preset shell_safe\n}\nagent a {\n    model m/x\n}\n",
			needle: "gate", next: "agent a",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out, diags := Format("m.agent", tc.src)
			if len(diags) > 0 {
				t.Fatalf("diags: %s", diags.Error())
			}
			ci := strings.Index(out, "// "+tc.needle)
			if ci < 0 {
				t.Fatalf("comment %q dropped:\n%s", tc.needle, out)
			}
			if di := strings.Index(out, tc.next); di >= 0 && ci > di {
				t.Fatalf("comment leaked past %q:\n%s", tc.next, out)
			}
			if out2, _ := Format("m.agent", out); out2 != out {
				t.Fatalf("not idempotent:\n%s", out)
			}
		})
	}
}

// Regression for the PR #516 sixth review: in a split-brace layout the body's `{` is on its own line
// (a workflow with header clauses, or `agent a\n{`), so the `{`-token key never matches the keyword
// line the printer passes to blockTail. A leftover body comment must still stay inside its resource —
// caught by registration under the enclosing top-level declaration's keyword line.
func TestFormat_splitBraceBodyCommentStaysInResource(t *testing.T) {
	cases := map[string]struct{ src, needle, next string }{
		"workflow": {
			src:    "workflow hello(input: string) -> string\n    policy default\n{\n    return hello(input)\n    // leftover\n}\npolicy default {\n    preset shell_safe\n}\n",
			needle: "leftover", next: "preset shell_safe",
		},
		"agent": {
			src:    "agent a\n{\n    model m/x\n    // leftover\n}\npolicy p {\n    preset shell_safe\n}\n",
			needle: "leftover", next: "policy p",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out, diags := Format("m.agent", tc.src)
			if len(diags) > 0 {
				t.Fatalf("diags: %s", diags.Error())
			}
			ci := strings.Index(out, "// "+tc.needle)
			if ci < 0 {
				t.Fatalf("comment %q dropped:\n%s", tc.needle, out)
			}
			if di := strings.Index(out, tc.next); di >= 0 && ci > di {
				t.Fatalf("comment leaked past %q:\n%s", tc.next, out)
			}
			if out2, _ := Format("m.agent", out); out2 != out {
				t.Fatalf("not idempotent:\n%s", out)
			}
		})
	}
}
