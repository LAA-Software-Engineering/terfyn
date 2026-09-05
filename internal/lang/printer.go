package lang

import (
	"fmt"
	"strconv"
	"strings"
)

// Print reconstructs canonical .agent source from an AST. Output is normalized —
// 4-space indentation, single spaces around operators, comma-joined effects — so
// it does not depend on the incidental formatting of the input. Print is the
// engine behind `terfyn fmt`; parse -> Print -> parse -> Print is stable
// (idempotent) for any file that parses without error.
func Print(f *File) string {
	if f == nil {
		return ""
	}
	p := newPrinter(f.cidx)
	for i, d := range f.Decls {
		// Blank-line separator first, then the comments that sit above this declaration (the file
		// header and per-decl doc comments), so a leading comment stays glued to what it documents:
		// `decl1\n\n// doc\ndecl2`.
		if i > 0 {
			p.WriteString("\n")
		}
		p.leadingBefore(declLine(d), "")
		switch n := d.(type) {
		case *AgentDecl:
			printAgent(p, n)
		case *WorkflowDecl:
			printWorkflow(p, n)
		case *ToolDecl:
			printTool(p, n)
		case *PolicyDecl:
			printPolicy(p, n)
		case *EnvironmentDecl:
			printEnvironment(p, n)
		case *ProviderDecl:
			printProvider(p, n)
		case *DefaultsDecl:
			printDefaults(p, n)
		case *LimitsDecl:
			printLimitsDecl(p, n)
		default:
			fmt.Fprintf(p, "/* unknown decl %T */\n", d)
		}
	}
	// Comments after the last declaration (a trailing file footer) must survive too.
	p.flushRemaining("")
	return p.String()
}

// declLine is the source line a top-level declaration starts on, used to gather the comments above it.
func declLine(d Decl) int {
	if d == nil {
		return 0
	}
	return d.Position().Line
}

// Format parses src and returns canonical .agent source plus any diagnostics.
// When parsing reports errors the returned source is best-effort (formatted from
// the partial AST) and callers should surface the diagnostics rather than write
// the output.
func Format(file, src string) (string, Diagnostics) {
	f, diags := Parse(file, src)
	return Print(f), diags
}

func printAgent(p *printer, a *AgentDecl) {
	fmt.Fprintf(p, "agent %s {\n", identName(a.Name))
	if a.Model != nil {
		p.leadingBefore(a.Model.Pos.Line, "    ")
		p.field("    ", fmt.Sprintf("model %s/%s", a.Model.Provider, a.Model.Name), a.Model.Pos.Line)
	}
	if a.Policy != nil {
		p.leadingBefore(a.Policy.Pos.Line, "    ")
		p.field("    ", "policy "+a.Policy.Name, a.Policy.Pos.Line)
	}
	if a.Description != nil {
		p.leadingBefore(a.Description.Pos.Line, "    ")
		printStringField(p, "    ", "description", a.Description.Value, a.Description.Pos.Line)
	}
	if a.Instructions != nil {
		p.leadingBefore(a.Instructions.Pos.Line, "    ")
		printInstructions(p, a.Instructions.Value, a.Instructions.Pos.Line)
	}
	if a.InstructionsFile != nil && a.InstructionsFile.Path != nil {
		p.leadingBefore(a.InstructionsFile.Pos.Line, "    ")
		// Re-emit the file("...") form verbatim (never the resolved prompt text), so fmt
		// round-trips the reference (#360).
		p.field("    ", fmt.Sprintf("instructions file(%s)", strconv.Quote(a.InstructionsFile.Path.Value)), a.InstructionsFile.Pos.Line)
	}
	if a.Constraints != nil {
		p.leadingBefore(a.Constraints.Pos.Line, "    ")
		printConstraints(p, a.Constraints)
	}
	if len(a.Grants) > 0 {
		p.leadingBefore(a.GrantsPos.Line, "    ")
		p.WriteString("    grants {\n")
		for _, g := range a.Grants {
			p.leadingBefore(g.Pos.Line, "        ")
			p.field("        ", dottedName(g.Segments), g.Pos.Line)
		}
		p.blockTail(a.GrantsPos.Line, "        ")
		p.WriteString("    }\n")
	}
	if a.Input != nil {
		p.leadingBefore(a.Input.Pos.Line, "    ")
		p.field("    ", "input "+a.Input.Name, a.Input.Pos.Line)
	}
	if a.Output != nil {
		p.leadingBefore(a.Output.Pos.Line, "    ")
		p.field("    ", "output "+a.Output.Name, a.Output.Pos.Line)
	}
	p.blockTail(a.Pos.Line, "    ")
	p.WriteString("}\n")
}

// printInstructions renders the agent prompt. A multiline value prints as a
// canonical `"""…"""` block whose body lines carry the field's 4-space indent and
// whose closing delimiter is on its own indented line — the exact shape
// normalizeMultiline strips back to the original value, so parse -> print -> parse
// is stable. A value with no newline — OR one containing a literal `"""`, which
// the raw multiline body cannot represent (it would read as a premature close and
// corrupt the file) — falls back to the escaped single-quoted form, which escapes
// newlines and quotes and always re-parses.
func printInstructions(p *printer, v string, line int) {
	printStringField(p, "    ", "instructions", v, line)
}

// printStringField renders a string-valued field (instructions, description) at the
// given indent: a single-line value as a quoted literal; a multiline value (or one
// containing a `"""`, which the raw block cannot hold) as a canonical `"""…"""` block
// whose body lines carry the field indent — the exact shape normalizeMultiline strips
// back, so parse -> print -> parse is stable.
func printStringField(p *printer, indent, name, v string, line int) {
	if !strings.Contains(v, "\n") || strings.Contains(v, `"""`) {
		// Single-line value: it can carry a trailing comment on its source line.
		p.field(indent, fmt.Sprintf("%s %s", name, strconv.Quote(v)), line)
		return
	}
	fmt.Fprintf(p, "%s%s \"\"\"\n", indent, name)
	for _, ln := range strings.Split(v, "\n") {
		if ln == "" {
			p.WriteString("\n")
		} else {
			p.WriteString(indent + ln + "\n")
		}
	}
	fmt.Fprintf(p, "%s\"\"\"\n", indent)
}

// printConstraints renders the `constraints { }` block, one field per line in a
// stable order, omitting fields the author did not set.
func printConstraints(p *printer, c *Constraints) {
	// A trailing comment on a single-line source block (`constraints { … } // note`) attaches to the
	// opening line, which fmt keeps even as it expands the block onto multiple lines.
	p.WriteString("    constraints {")
	p.trailingOn(c.Pos.Line)
	p.WriteString("\n")
	if c.MaxIterations != nil {
		fmt.Fprintf(p, "        maxIterations %d\n", *c.MaxIterations)
	}
	if c.MaxTokens != nil {
		fmt.Fprintf(p, "        maxTokens %d\n", *c.MaxTokens)
	}
	if c.TimeoutSeconds != nil {
		fmt.Fprintf(p, "        timeoutSeconds %d\n", *c.TimeoutSeconds)
	}
	if c.Temperature != nil {
		fmt.Fprintf(p, "        temperature %s\n", strconv.FormatFloat(*c.Temperature, 'g', -1, 64))
	}
	if c.RequireStructuredOutput != nil {
		fmt.Fprintf(p, "        requireStructuredOutput %s\n", strconv.FormatBool(*c.RequireStructuredOutput))
	}
	p.blockTail(c.Pos.Line, "        ")
	p.WriteString("    }\n")
}

func printWorkflow(p *printer, w *WorkflowDecl) {
	params := make([]string, len(w.Params))
	for i, p := range w.Params {
		params[i] = fmt.Sprintf("%s: %s", identName(p.Name), typeName(p.Type))
	}
	fmt.Fprintf(p, "workflow %s(%s)", identName(w.Name), strings.Join(params, ", "))
	if w.Result != nil {
		fmt.Fprintf(p, " -> %s", w.Result.Name)
	}
	var clauses []string
	if w.Policy != nil {
		clauses = append(clauses, fmt.Sprintf("policy %s", w.Policy.Name))
	}
	if w.Effects != nil {
		names := make([]string, len(w.Effects))
		for i, e := range w.Effects {
			names[i] = e.Name
		}
		clauses = append(clauses, fmt.Sprintf("effects { %s }", strings.Join(names, ", ")))
	}
	if w.Description != nil {
		// A description (possibly multiline) does not fit the inline header, so the
		// header clauses go on their own indented lines before the opening brace.
		p.WriteString("\n")
		printStringField(p, "    ", "description", w.Description.Value, w.Description.Pos.Line)
		for _, c := range clauses {
			fmt.Fprintf(p, "    %s\n", c)
		}
		p.WriteString("{\n")
	} else {
		for _, c := range clauses {
			fmt.Fprintf(p, " %s", c)
		}
		p.WriteString(" {\n")
	}
	for _, s := range w.Body {
		printStmt(p, s, 1)
	}
	p.blockTail(w.Pos.Line, "    ")
	p.WriteString("}\n")
}

func printStmt(p *printer, s Stmt, depth int) {
	indent := strings.Repeat("    ", depth)
	// Comments authored above this statement attach to it, at the statement's indent.
	p.leadingBefore(s.Position().Line, indent)
	switch n := s.(type) {
	case *AssignStmt:
		p.field(indent, fmt.Sprintf("%s = %s", identName(n.Target), printExpr(n.Value)), n.Pos.Line)
	case *ExprStmt:
		p.field(indent, printExpr(n.X), n.Pos.Line)
	case *ReturnStmt:
		p.field(indent, "return "+printExpr(n.Value), n.Pos.Line)
	case *ParallelStmt:
		fmt.Fprintf(p, "%sparallel {\n", indent)
		for _, a := range n.Body {
			printStmt(p, a, depth+1)
		}
		p.blockTail(n.Pos.Line, indent+"    ")
		fmt.Fprintf(p, "%s}\n", indent)
	case *IfStmt:
		printIf(p, n, depth)
	case *ForStmt:
		kw := "for"
		if n.Parallel {
			kw = "parallel for"
		}
		fmt.Fprintf(p, "%s%s %s in %s {\n", indent, kw, identName(n.Var), printExpr(n.In))
		for _, st := range n.Body {
			printStmt(p, st, depth+1)
		}
		p.blockTail(n.Pos.Line, indent+"    ")
		fmt.Fprintf(p, "%s}\n", indent)
	case *WhileStmt:
		fmt.Fprintf(p, "%swhile %s limit %d {\n", indent, printExpr(n.Cond), n.Limit)
		for _, st := range n.Body {
			printStmt(p, st, depth+1)
		}
		p.blockTail(n.Pos.Line, indent+"    ")
		fmt.Fprintf(p, "%s}\n", indent)
	case *RetryStmt:
		fmt.Fprintf(p, "%sretry until %s limit %d {\n", indent, printExpr(n.Cond), n.Limit)
		for _, st := range n.Body {
			printStmt(p, st, depth+1)
		}
		p.blockTail(n.Pos.Line, indent+"    ")
		fmt.Fprintf(p, "%s}\n", indent)
	case *ApprovalStmt:
		inner := indent + "    "
		fmt.Fprintf(p, "%sapproval %s {\n", indent, identName(n.Bind))
		if n.Description != nil {
			printStringField(p, inner, "description", n.Description.Value, n.Description.Pos.Line)
		}
		if len(n.RedactKeys) > 0 {
			fmt.Fprintf(p, "%sredactKeys {", inner)
			for _, k := range n.RedactKeys {
				fmt.Fprintf(p, " %s", strconv.Quote(k.Value))
			}
			p.WriteString(" }\n")
		}
		if len(n.With) > 0 {
			fmt.Fprintf(p, "%swith {\n", inner)
			arg := inner + "    "
			for _, a := range n.With {
				if a.Name != nil {
					fmt.Fprintf(p, "%s%s: %s\n", arg, identName(a.Name), printExpr(a.Value))
				} else {
					fmt.Fprintf(p, "%s%s\n", arg, printExpr(a.Value))
				}
			}
			fmt.Fprintf(p, "%s}\n", inner)
		}
		fmt.Fprintf(p, "%s}\n", indent)
	default:
		fmt.Fprintf(p, "%s/* unknown stmt %T */\n", indent, s)
	}
}

// printIf renders a conditional, collapsing `else { if … }` back into
// `else if …` when the else arm is exactly one nested IfStmt.
func printIf(p *printer, n *IfStmt, depth int) {
	indent := strings.Repeat("    ", depth)
	fmt.Fprintf(p, "%sif %s {\n", indent, printExpr(n.Cond))
	for _, st := range n.Then {
		printStmt(p, st, depth+1)
	}
	if len(n.Else) == 1 {
		if elif, ok := n.Else[0].(*IfStmt); ok {
			fmt.Fprintf(p, "%s} else ", indent)
			// Render the nested if without its leading indent, on the same line. The nested printer
			// shares the comment index and the emitted-flags slice, so a comment inside the else-if
			// body is emitted exactly once.
			nested := &printer{idx: p.idx, emitted: p.emitted}
			printIf(nested, elif, depth)
			p.WriteString(strings.TrimLeft(nested.String(), " "))
			return
		}
	}
	if len(n.Else) > 0 {
		fmt.Fprintf(p, "%s} else {\n", indent)
		for _, st := range n.Else {
			printStmt(p, st, depth+1)
		}
	}
	p.blockTail(n.Pos.Line, indent+"    ")
	fmt.Fprintf(p, "%s}\n", indent)
}

func printExpr(e Expr) string {
	switch n := e.(type) {
	case *RefExpr:
		return dottedName(n.Parts)
	case *LitExpr:
		return printLit(n)
	case *UnaryExpr:
		// `!` binds tighter than every binary operator, so any binary operand is
		// parenthesized; a unary/leaf operand is not.
		return "!" + parenIfBinary(n.X)
	case *BinaryExpr:
		return fmt.Sprintf("%s %s %s", printBinOperand(n.X, n.Op, false), opSymbol(n.Op), printBinOperand(n.Y, n.Op, true))
	case *CallExpr:
		args := make([]string, len(n.Args))
		for i, a := range n.Args {
			if a.Name != nil {
				args[i] = fmt.Sprintf("%s: %s", a.Name.Name, printExpr(a.Value))
			} else {
				args[i] = printExpr(a.Value)
			}
		}
		return fmt.Sprintf("%s(%s)", dottedName(n.Callee.Parts), strings.Join(args, ", "))
	case *ObjectExpr:
		if len(n.Fields) == 0 {
			return "{}"
		}
		fields := make([]string, len(n.Fields))
		for i, f := range n.Fields {
			fields[i] = fmt.Sprintf("%s: %s", identName(f.Key), printExpr(f.Value))
		}
		return "{ " + strings.Join(fields, ", ") + " }"
	case nil:
		return "/*nil*/"
	default:
		return fmt.Sprintf("/* unknown expr %T */", e)
	}
}

// binPrec is the binding tightness used to decide parenthesization: `||` loosest,
// then `&&`, then comparisons (tightest of the binaries). Higher binds tighter.
func binPrec(k Kind) int {
	switch k {
	case KindOrOr:
		return 1
	case KindAndAnd:
		return 2
	default: // comparisons (==, !=, <, <=, >, >=) — non-chaining
		return 3
	}
}

// printBinOperand renders one operand of a binary expression, adding parentheses
// only when omitting them would regroup the tree on re-parse. Operators are
// left-associative, so an operand of equal precedence needs parentheses only on
// the right. A non-binary operand (a unary, call, ref, or literal) never needs
// them.
func printBinOperand(e Expr, parentOp Kind, isRight bool) string {
	be, ok := e.(*BinaryExpr)
	if !ok {
		return printExpr(e)
	}
	cp, pp := binPrec(be.Op), binPrec(parentOp)
	if cp < pp || (cp == pp && isRight) {
		return "(" + printExpr(e) + ")"
	}
	return printExpr(e)
}

// parenIfBinary parenthesizes a binary operand (used by unary `!`, which binds
// tighter than any binary).
func parenIfBinary(e Expr) string {
	if _, ok := e.(*BinaryExpr); ok {
		return "(" + printExpr(e) + ")"
	}
	return printExpr(e)
}

func printLit(n *LitExpr) string {
	switch v := n.Value.(type) {
	case string:
		return strconv.Quote(v)
	case bool:
		return strconv.FormatBool(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	default:
		return fmt.Sprintf("%v", n.Value)
	}
}

func opSymbol(k Kind) string {
	switch k {
	case KindEqEq:
		return "=="
	case KindBangEq:
		return "!="
	case KindLt:
		return "<"
	case KindLte:
		return "<="
	case KindGt:
		return ">"
	case KindGte:
		return ">="
	case KindAndAnd:
		return "&&"
	case KindOrOr:
		return "||"
	default:
		return "?"
	}
}

func identName(i *Ident) string {
	if i == nil {
		return "_"
	}
	return i.Name
}

func typeName(t *TypeRef) string {
	if t == nil {
		return "_"
	}
	return t.Name
}
