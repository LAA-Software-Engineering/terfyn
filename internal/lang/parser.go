package lang

import (
	"strconv"
	"strings"
)

// Parse lexes and parses src into a typed AST. It always returns a non-nil
// *File (possibly with a partial or empty Decls slice) plus every diagnostic
// found — lexical and syntactic — sorted by position. The parser recovers after
// each error (see the sync* helpers) so malformed input yields multiple
// positioned diagnostics rather than stopping at the first (issue #196).
//
// Parsing does not resolve references, check types, or verify the effects
// clause; those are #198, and lowering to the resource model is #197.
func Parse(file, src string) (*File, Diagnostics) {
	p := newParser(file, src)
	f := p.parseFile()
	// Comments are collected by the lexer as it scans; by parse end the whole stream has been
	// consumed, so l.Comments() holds every comment in source order for the formatter (issue #509).
	if f != nil {
		f.Comments = p.lex.Comments()
		f.cidx = buildCommentIndex(src, f.Comments)
	}
	diags := append(p.lex.Diagnostics(), p.diags...)
	return f, diags.Sorted()
}

type parser struct {
	lex   *Lexer
	file  string
	cur   Token // current token
	peekt Token // one-token lookahead
	diags Diagnostics
}

func newParser(file, src string) *parser {
	p := &parser{lex: NewLexer(file, src), file: file}
	// Prime cur and peekt.
	p.cur = p.lex.Next()
	p.peekt = p.lex.Next()
	return p
}

// advance consumes the current token, shifting the lookahead forward.
func (p *parser) advance() {
	p.cur = p.peekt
	p.peekt = p.lex.Next()
}

func (p *parser) errorf(pos Pos, format string, args ...any) {
	p.diags = append(p.diags, diagf(pos, format, args...))
}

// expect consumes cur if it is of kind k; otherwise it records a diagnostic and
// leaves cur in place (the caller's loop or a sync* helper makes progress).
func (p *parser) expect(k Kind, context string) (Token, bool) {
	if p.cur.Kind == k {
		t := p.cur
		p.advance()
		return t, true
	}
	p.errorf(p.cur.Pos, "expected %s %s, got %s", k, context, p.cur)
	return p.cur, false
}

// ident consumes and returns an identifier node, or records a diagnostic and
// returns nil without consuming.
func (p *parser) ident(context string) *Ident {
	if p.cur.Kind == KindIdent {
		id := &Ident{Pos: p.cur.Pos, Name: p.cur.Lit}
		p.advance()
		return id
	}
	p.errorf(p.cur.Pos, "expected identifier %s, got %s", context, p.cur)
	return nil
}

// --- Top level --------------------------------------------------------------

func (p *parser) parseFile() *File {
	f := &File{Pos: p.cur.Pos}
	for p.cur.Kind != KindEOF {
		switch {
		case p.cur.Kind == KindAgent:
			f.Decls = append(f.Decls, p.parseAgent())
		case p.cur.Kind == KindWorkflow:
			f.Decls = append(f.Decls, p.parseWorkflow())
		case p.cur.Kind == KindIdent && p.cur.Lit == "tool":
			f.Decls = append(f.Decls, p.parseTool())
		case p.cur.Kind == KindIdent && p.cur.Lit == "policy":
			f.Decls = append(f.Decls, p.parsePolicy())
		case p.cur.Kind == KindIdent && p.cur.Lit == "environment":
			f.Decls = append(f.Decls, p.parseEnvironment())
		case p.cur.Kind == KindIdent && p.cur.Lit == "provider":
			f.Decls = append(f.Decls, p.parseProvider())
		case p.cur.Kind == KindIdent && p.cur.Lit == "defaults":
			f.Decls = append(f.Decls, p.parseDefaults())
		case p.cur.Kind == KindIdent && p.cur.Lit == "limits":
			f.Decls = append(f.Decls, p.parseLimitsDecl())
		default:
			p.errorf(p.cur.Pos, "expected 'agent', 'workflow', 'tool', 'policy', 'environment', 'provider', 'defaults', or 'limits' declaration, got %s", p.cur)
			p.syncTopLevel()
		}
	}
	return f
}

// syncTopLevel skips to the next top-level declaration keyword or EOF, always
// consuming at least one token so parseFile cannot loop.
func (p *parser) syncTopLevel() {
	p.advance()
	for p.cur.Kind != KindEOF && p.cur.Kind != KindAgent && p.cur.Kind != KindWorkflow && !p.isResourceDeclKeyword() {
		p.advance()
	}
}

// syncLine consumes the remainder of the current source line, stopping before
// the first token of the next line, a closing brace, or EOF. It is the recovery
// point for line-oriented contexts (agent fields, grants, effects, statements)
// and always consumes at least one token.
func (p *parser) syncLine() {
	line := p.cur.Pos.Line
	p.advance()
	for p.cur.Kind != KindEOF && p.cur.Kind != KindRBrace {
		if p.cur.Pos.Line != line {
			return
		}
		p.advance()
	}
}

// syncList recovers inside a comma-separated, parenthesized list (params,
// args): it advances to the next comma (consumed) or the closing paren / EOF
// (left in place).
func (p *parser) syncList() {
	for {
		switch p.cur.Kind {
		case KindEOF, KindRParen:
			return
		case KindComma:
			p.advance()
			return
		default:
			p.advance()
		}
	}
}

// --- Agent ------------------------------------------------------------------

func (p *parser) parseAgent() *AgentDecl {
	decl := &AgentDecl{Pos: p.cur.Pos}
	p.advance() // consume 'agent'
	decl.Name = p.ident("after 'agent'")
	if _, ok := p.expect(KindLBrace, "to open agent body"); !ok {
		return decl
	}
	// seen tracks which fields have appeared so a repeated field is reported
	// rather than silently overwriting (each agent field is admitted once).
	seen := map[string]bool{}
	dup := func(field string, pos Pos) bool {
		if seen[field] {
			p.errorf(pos, "duplicate agent field %q (each field may appear at most once)", field)
			return true
		}
		seen[field] = true
		return false
	}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected agent field (model, policy, description, instructions, constraints, grants, input, output), got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		switch field {
		case "model":
			p.advance()
			if m := p.parseModelRef(); !dup(field, fpos) {
				decl.Model = m
			}
		case "policy":
			p.advance()
			if id := p.ident("after 'policy'"); !dup(field, fpos) {
				decl.Policy = id
			}
		case "description":
			p.advance()
			if s := p.parseStringLit("after 'description'"); !dup(field, fpos) {
				decl.Description = s
			}
		case "instructions":
			p.advance()
			// instructions accepts an inline string literal OR a load-time file reference,
			// file("path") (#360). `file` is a contextual identifier followed by '('.
			if p.cur.Kind == KindIdent && p.cur.Lit == "file" {
				if fr := p.parseFileRef(); !dup(field, fpos) {
					decl.InstructionsFile = fr
				}
			} else if s := p.parseStringLit("after 'instructions'"); !dup(field, fpos) {
				decl.Instructions = s
			}
		case "constraints":
			p.advance()
			if c := p.parseConstraints(); !dup(field, fpos) {
				decl.Constraints = c
			}
		case "grants":
			p.advance()
			if g := p.parseGrants(); !dup(field, fpos) {
				decl.Grants = g
				decl.GrantsPos = fpos // the `grants` keyword line, so the formatter can anchor a doc comment above the block (issue #509)
			}
		case "input":
			p.advance()
			if t := p.parseTypeRef("after 'input'"); !dup(field, fpos) {
				decl.Input = t
			}
		case "output":
			p.advance()
			if t := p.parseTypeRef("after 'output'"); !dup(field, fpos) {
				decl.Output = t
			}
		default:
			p.errorf(fpos, "unknown agent field %q (want model, policy, description, instructions, constraints, grants, input, or output)", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close agent body")
	return decl
}

// parseStringLit consumes a string literal token (single- or triple-quoted; the
// lexer has already decoded and, for the multiline form, normalized it). where
// names the context for the diagnostic on a missing string.
func (p *parser) parseStringLit(where string) *StringLit {
	if p.cur.Kind != KindString {
		p.errorf(p.cur.Pos, "expected a string literal %s, got %s", where, p.cur)
		return nil
	}
	s := &StringLit{Pos: p.cur.Pos, Value: p.cur.Lit}
	p.advance()
	return s
}

// parseFileRef parses `file("path")` — a load-time file reference (#360). The current token is the
// contextual identifier `file`. The path is a string literal; the parser does not read it (the
// project loader resolves and reads it relative to the .agent file, within the project root).
func (p *parser) parseFileRef() *InstructionsFile {
	fr := &InstructionsFile{Pos: p.cur.Pos}
	p.advance() // consume 'file'
	if _, ok := p.expect(KindLParen, "after 'file' in a file(...) reference"); !ok {
		return fr
	}
	fr.Path = p.parseStringLit("for the path inside file(...)")
	p.expect(KindRParen, "to close a file(...) reference")
	return fr
}

// parseConstraints parses `{ maxIterations N timeoutSeconds N ... }` — the fixed
// set of agent execution bounds (#310). Each field appears at most once; an unknown
// field or a wrong value type is a diagnostic. The field words are contextual.
func (p *parser) parseConstraints() *Constraints {
	c := &Constraints{Pos: p.cur.Pos}
	if _, ok := p.expect(KindLBrace, "to open constraints block"); !ok {
		return c
	}
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected a constraint field (maxIterations, maxTokens, timeoutSeconds, temperature, requireStructuredOutput), got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		p.advance()
		if seen[field] {
			p.errorf(fpos, "duplicate constraint %q (each appears at most once)", field)
		}
		seen[field] = true
		switch field {
		case "maxIterations":
			if n, ok := p.constraintInt(field); ok {
				c.MaxIterations = &n
			}
		case "maxTokens":
			if n, ok := p.constraintInt(field); ok {
				c.MaxTokens = &n
			}
		case "timeoutSeconds":
			if n, ok := p.constraintInt(field); ok {
				c.TimeoutSeconds = &n
			}
		case "temperature":
			if f, ok := p.constraintFloat(field); ok {
				c.Temperature = &f
			}
		case "requireStructuredOutput":
			if b, ok := p.constraintBool(field); ok {
				c.RequireStructuredOutput = &b
			}
		default:
			p.errorf(fpos, "unknown constraint %q (want maxIterations, maxTokens, timeoutSeconds, temperature, or requireStructuredOutput)", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close constraints block")
	return c
}

func (p *parser) constraintInt(field string) (int, bool) {
	if p.cur.Kind != KindNumber || strings.Contains(p.cur.Lit, ".") {
		p.errorf(p.cur.Pos, "constraint %q wants a positive integer, got %s", field, p.cur)
		p.syncLine()
		return 0, false
	}
	n, err := strconv.ParseInt(p.cur.Lit, 10, 64)
	if err != nil || n <= 0 {
		p.errorf(p.cur.Pos, "constraint %q wants a positive integer, got %q", field, p.cur.Lit)
		p.advance()
		return 0, false
	}
	p.advance()
	return int(n), true
}

func (p *parser) constraintFloat(field string) (float64, bool) {
	if p.cur.Kind != KindNumber {
		p.errorf(p.cur.Pos, "constraint %q wants a number, got %s", field, p.cur)
		p.syncLine()
		return 0, false
	}
	f, err := strconv.ParseFloat(p.cur.Lit, 64)
	if err != nil {
		p.errorf(p.cur.Pos, "constraint %q: invalid number %q", field, p.cur.Lit)
		p.advance()
		return 0, false
	}
	p.advance()
	return f, true
}

func (p *parser) constraintBool(field string) (bool, bool) {
	if p.cur.Kind == KindIdent && (p.cur.Lit == "true" || p.cur.Lit == "false") {
		v := p.cur.Lit == "true"
		p.advance()
		return v, true
	}
	p.errorf(p.cur.Pos, "constraint %q wants true or false, got %s", field, p.cur)
	p.syncLine()
	return false, false
}

// parseModelRef parses <provider>/<name> (e.g. openai/gpt-5).
func (p *parser) parseModelRef() *ModelRef {
	if p.cur.Kind != KindIdent {
		p.errorf(p.cur.Pos, "expected model provider identifier, got %s", p.cur)
		return nil
	}
	m := &ModelRef{Pos: p.cur.Pos, Provider: p.cur.Lit}
	p.advance()
	if _, ok := p.expect(KindSlash, "in model reference (want <provider>/<name>)"); !ok {
		m.Raw = m.Provider
		return m
	}
	if p.cur.Kind != KindIdent {
		p.errorf(p.cur.Pos, "expected model name after '/', got %s", p.cur)
		m.Raw = m.Provider + "/"
		return m
	}
	m.Name = p.cur.Lit
	m.Raw = m.Provider + "/" + m.Name
	p.advance()
	return m
}

// parseGrants parses `{ tool.<name>.<operation> ... }`. Grants are
// newline-separated in the ADR surface; an optional comma between grants is
// tolerated for symmetry with the effects clause.
func (p *parser) parseGrants() []*Grant {
	if _, ok := p.expect(KindLBrace, "to open grants block"); !ok {
		return nil
	}
	var grants []*Grant
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected grant tool.<name>.<operation>, got %s", p.cur)
			p.syncLine()
			continue
		}
		grants = append(grants, p.parseGrant())
		if p.cur.Kind == KindComma {
			p.advance()
		}
	}
	p.expect(KindRBrace, "to close grants block")
	return grants
}

// parseGrant parses one dotted path and validates the grant shape. A grant
// names a concrete operation as tool.<name>.<operation> (ADR 002 amendment,
// #188) and must be textually distinguishable from a bare dotted effect: the
// leading "tool" segment is required. The split matches tools.ParseUses — the
// tool name is the single segment after "tool", and the operation is every
// segment after that (at least one, possibly dotted, e.g.
// tool.github.pull_request.post_comment → name github, operation
// pull_request.post_comment).
func (p *parser) parseGrant() *Grant {
	parts := p.parseDottedPath("in grant")
	g := &Grant{Segments: parts}
	if len(parts) == 0 {
		return g
	}
	g.Pos = parts[0].Pos
	switch {
	case parts[0].Name != "tool":
		p.errorf(g.Pos, "grant must name a concrete operation as tool.<name>.<operation>; %q is not a grant (bare dotted names are effects)", dottedName(parts))
	case len(parts) < 3:
		p.errorf(g.Pos, "grant must be tool.<name>.<operation> with a tool name and an operation, got %q", dottedName(parts))
	default:
		g.Name = parts[1]
		g.Operation = parts[2:]
	}
	return g
}

// --- Workflow ---------------------------------------------------------------

func (p *parser) parseWorkflow() *WorkflowDecl {
	decl := &WorkflowDecl{Pos: p.cur.Pos}
	p.advance() // consume 'workflow'
	decl.Name = p.ident("after 'workflow'")
	decl.Params = p.parseParams()
	if p.cur.Kind == KindArrow {
		p.advance()
		decl.Result = p.parseTypeRef("after '->'")
	}
	// Optional header clauses before the body: `description "..."` and the
	// `effects { }` clause, in either order (each contextual, each at most once).
	for {
		if p.cur.Kind == KindIdent && p.cur.Lit == "description" && decl.Description == nil {
			p.advance()
			decl.Description = p.parseStringLit("after workflow 'description'")
			continue
		}
		if p.cur.Kind == KindIdent && p.cur.Lit == "policy" && decl.Policy == nil {
			p.advance()
			decl.Policy = p.ident("after workflow 'policy'")
			continue
		}
		if p.cur.Kind == KindIdent && p.cur.Lit == "effects" && decl.Effects == nil {
			p.advance()
			decl.Effects = p.parseEffects()
			continue
		}
		break
	}
	decl.Body = p.parseBlock("to open workflow body")
	return decl
}

func (p *parser) parseParams() []*Param {
	if _, ok := p.expect(KindLParen, "to open parameter list"); !ok {
		return nil
	}
	var params []*Param
	for p.cur.Kind != KindRParen && p.cur.Kind != KindEOF {
		name := p.ident("parameter name")
		if name == nil {
			p.syncList()
			continue
		}
		p.expect(KindColon, "after parameter name")
		typ := p.parseTypeRef("as parameter type")
		params = append(params, &Param{Pos: name.Pos, Name: name, Type: typ})
		if p.cur.Kind == KindComma {
			p.advance()
			continue
		}
		break
	}
	p.expect(KindRParen, "to close parameter list")
	return params
}

func (p *parser) parseTypeRef(context string) *TypeRef {
	if p.cur.Kind != KindIdent {
		p.errorf(p.cur.Pos, "expected type name %s, got %s", context, p.cur)
		return nil
	}
	t := &TypeRef{Pos: p.cur.Pos, Name: p.cur.Lit}
	p.advance()
	return t
}

// parseEffects parses `{ github.read, github.write, ... }`. Effects are bare
// dotted identifiers (no tool. prefix). Commas between effects are optional so
// the block reads the same whether authored comma- or newline-separated.
func (p *parser) parseEffects() []*EffectRef {
	if _, ok := p.expect(KindLBrace, "to open effects block"); !ok {
		return nil
	}
	var effs []*EffectRef
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected effect identifier, got %s", p.cur)
			p.syncLine()
			continue
		}
		effs = append(effs, p.parseEffectRef())
		if p.cur.Kind == KindComma {
			p.advance()
		}
	}
	p.expect(KindRBrace, "to close effects block")
	return effs
}

func (p *parser) parseEffectRef() *EffectRef {
	parts := p.parseDottedPath("in effects clause")
	if len(parts) == 0 {
		return nil
	}
	e := &EffectRef{Pos: parts[0].Pos, Name: dottedName(parts)}
	if parts[0].Name == "tool" {
		p.errorf(e.Pos, "effect identifiers are bare dotted names; %q looks like a grant (grants use tool.<name>.<operation>)", e.Name)
	}
	return e
}

// parseBlock parses a `{ statement* }` block (a workflow body, or a conditional
// or loop body, #199). context describes what the opening brace opens, for the
// diagnostic. When the opening brace is absent it records a diagnostic and
// returns nil rather than consuming unrelated tokens (top-level recovery then
// takes over).
func (p *parser) parseBlock(context string) []Stmt {
	if _, ok := p.expect(KindLBrace, context); !ok {
		return nil
	}
	var stmts []Stmt
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		s := p.parseStmt()
		if s == nil {
			p.syncLine()
			continue
		}
		stmts = append(stmts, s)
	}
	p.expect(KindRBrace, "to close block")
	return stmts
}

func (p *parser) parseStmt() Stmt {
	switch p.cur.Kind {
	case KindReturn:
		pos := p.cur.Pos
		p.advance()
		return &ReturnStmt{Pos: pos, Value: p.parseExpr()}
	case KindIf:
		return p.parseIf()
	case KindFor:
		return p.parseFor(false, p.cur.Pos)
	case KindWhile:
		return p.parseWhile()
	case KindParallel:
		// `parallel for x in coll { }` is dynamic fan-out (a loop, #199);
		// `parallel { a = ...; b = ... }` is static fan-out (#192).
		if p.peekt.Kind == KindFor {
			pos := p.cur.Pos
			p.advance() // consume 'parallel'
			return p.parseFor(true, pos)
		}
		return p.parseParallel()
	case KindIdent:
		// `retry until <cond> limit <N> { }` is the bounded-retry construct (#361). `retry`
		// and `until` are contextual keywords: only the `retry until` shape triggers it, so a
		// binding named `retry` (e.g. `retry = ...`) is still an ordinary assignment below.
		if p.cur.Lit == "retry" && p.peekt.Kind == KindIdent && p.peekt.Lit == "until" {
			return p.parseRetry()
		}
		// `approval <bind> { … }` is the workflow human-pause construct (#440). `approval` is a
		// contextual keyword: only `approval <ident>` opens it, so `approval = …` (peek '=') and a bare
		// call `approval(…)`/ref `approval.x` remain ordinary statements handled below.
		if p.cur.Lit == "approval" && p.peekt.Kind == KindIdent {
			return p.parseApproval()
		}
		// `name = expr` is an assignment; anything else is an expression
		// statement (a bare call for its effect).
		if p.peekt.Kind == KindEquals {
			target := p.ident("assignment target")
			p.advance() // '='
			return &AssignStmt{Pos: target.Pos, Target: target, Value: p.parseExpr()}
		}
		x := p.parseExpr()
		if x == nil {
			return nil
		}
		return &ExprStmt{Pos: x.Position(), X: x}
	default:
		p.errorf(p.cur.Pos, "expected statement (assignment, call, parallel, or return), got %s", p.cur)
		return nil
	}
}

// parseApproval parses `approval <bind> { description "…" redactKeys { "k1" "k2" } }` (#440). Both
// fields are optional; each may appear at most once.
func (p *parser) parseApproval() *ApprovalStmt {
	s := &ApprovalStmt{Pos: p.cur.Pos}
	p.advance() // consume 'approval'
	s.Bind = p.ident("after 'approval'")
	if _, ok := p.expect(KindLBrace, "to open approval body"); !ok {
		return s
	}
	seen := map[string]bool{}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected an approval field (description, redactKeys), got %s", p.cur)
			p.syncLine()
			continue
		}
		field, fpos := p.cur.Lit, p.cur.Pos
		p.advance()
		if seen[field] {
			p.errorf(fpos, "duplicate approval field %q", field)
		}
		seen[field] = true
		switch field {
		case "description":
			s.Description = p.parseStringLit("for description")
		case "redactKeys":
			s.RedactKeys = p.parseStringListBlock("redactKeys")
		case "with":
			s.With = p.parseApprovalWith()
		default:
			p.errorf(fpos, "unknown approval field %q (want description, redactKeys, or with)", field)
			p.syncLine()
		}
	}
	p.expect(KindRBrace, "to close approval body")
	return s
}

// parseApprovalWith parses the approval review-payload block `with { <name>: <expr> … }` — the same
// named-argument shape a call takes, so it lowers through the identical Args path (#440).
func (p *parser) parseApprovalWith() []*Arg {
	if _, ok := p.expect(KindLBrace, "to open with block"); !ok {
		return nil
	}
	var out []*Arg
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		a := p.parseArg()
		if a == nil {
			p.syncLine()
			continue
		}
		out = append(out, a)
		if p.cur.Kind == KindComma {
			p.advance()
		}
	}
	p.expect(KindRBrace, "to close with block")
	return out
}

func (p *parser) parseParallel() *ParallelStmt {
	stmt := &ParallelStmt{Pos: p.cur.Pos}
	p.advance() // consume 'parallel'
	if _, ok := p.expect(KindLBrace, "to open parallel block"); !ok {
		return stmt
	}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind == KindIdent && p.peekt.Kind == KindEquals {
			target := p.ident("branch name")
			p.advance() // '='
			stmt.Body = append(stmt.Body, &AssignStmt{Pos: target.Pos, Target: target, Value: p.parseExpr()})
			continue
		}
		p.errorf(p.cur.Pos, "parallel branches must be assignments (name = Call(...)), got %s", p.cur)
		p.syncLine()
	}
	p.expect(KindRBrace, "to close parallel block")
	return stmt
}

// parseIf parses `if <cond> { then } (else ({ else } | if ...))?` (#199). An
// `else if` chain is represented as an Else holding one nested IfStmt.
func (p *parser) parseIf() *IfStmt {
	stmt := &IfStmt{Pos: p.cur.Pos}
	p.advance() // consume 'if'
	stmt.Cond = p.parseExpr()
	stmt.Then = p.parseBlock("to open 'if' body")
	if p.cur.Kind == KindElse {
		p.advance() // consume 'else'
		if p.cur.Kind == KindIf {
			stmt.Else = []Stmt{p.parseIf()}
		} else {
			stmt.Else = p.parseBlock("to open 'else' body")
		}
	}
	return stmt
}

// parseFor parses `for <var> in <collection> { body }` (#199). pos is the
// keyword position (the `for`, or the `parallel` for the fan-out form).
// `in` is a contextual keyword matched here, not a reserved word, so a parameter
// may still be named `in`.
func (p *parser) parseFor(parallel bool, pos Pos) *ForStmt {
	stmt := &ForStmt{Pos: pos, Parallel: parallel}
	p.advance() // consume 'for'
	stmt.Var = p.ident("as loop variable after 'for'")
	if p.cur.Kind == KindIdent && p.cur.Lit == "in" {
		p.advance()
	} else {
		p.errorf(p.cur.Pos, "expected 'in' after loop variable, got %s", p.cur)
	}
	stmt.In = p.parseExpr()
	stmt.Body = p.parseBlock("to open loop body")
	return stmt
}

// parseWhile parses `while <cond> limit <N> { body }` (#288). `limit` is a
// contextual keyword matched here (an ordinary identifier), so a parameter may
// still be named `limit`. The bound N must be a positive integer literal — the
// language admits no unbounded effectful loop (ADR 002 §6) — so a missing, zero,
// fractional, or dynamic bound is a diagnostic.
func (p *parser) parseWhile() *WhileStmt {
	stmt := &WhileStmt{Pos: p.cur.Pos}
	p.advance() // consume 'while'
	stmt.Cond = p.parseExpr()
	if p.cur.Kind == KindIdent && p.cur.Lit == "limit" {
		p.advance()
		stmt.Limit = p.parseLimit("while")
	} else {
		p.errorf(p.cur.Pos, "expected 'limit' <positive integer> after the while condition (an unbounded while is not allowed), got %s", p.cur)
	}
	stmt.Body = p.parseBlock("to open while body")
	return stmt
}

// parseRetry parses `retry until <cond> limit <N> { body }` (#361). The current token is the
// contextual `retry` keyword and the lookahead is `until`. Like `while`, the bound N must be a
// positive integer literal — the language admits no unbounded effectful loop (ADR 002 §6).
func (p *parser) parseRetry() *RetryStmt {
	stmt := &RetryStmt{Pos: p.cur.Pos}
	p.advance() // consume 'retry'
	p.advance() // consume 'until' (verified by the caller's lookahead)
	stmt.Cond = p.parseExpr()
	if p.cur.Kind == KindIdent && p.cur.Lit == "limit" {
		p.advance()
		stmt.Limit = p.parseLimit("retry")
	} else {
		p.errorf(p.cur.Pos, "expected 'limit' <positive integer> after the retry-until condition (an unbounded retry is not allowed), got %s", p.cur)
	}
	stmt.Body = p.parseBlock("to open retry body")
	return stmt
}

// parseLimit consumes the mandatory positive-integer bound after `limit` for a bounded loop
// (`construct` names it for diagnostics). A non-number (e.g. a dynamic `limit input.max`), a
// fractional, or a non-positive value is a diagnostic; Limit is left 0 so the program is not
// treated as valid.
func (p *parser) parseLimit(construct string) int {
	tok := p.cur
	if tok.Kind != KindNumber {
		p.errorf(tok.Pos, "%s limit must be a positive integer literal, got %s (a dynamic or non-integer bound is not allowed)", construct, tok)
		return 0
	}
	p.advance()
	if strings.Contains(tok.Lit, ".") {
		p.errorf(tok.Pos, "%s limit must be a whole number, got %q", construct, tok.Lit)
		return 0
	}
	n, err := strconv.ParseInt(tok.Lit, 10, 64)
	if err != nil || n <= 0 {
		p.errorf(tok.Pos, "%s limit must be a positive integer, got %q", construct, tok.Lit)
		return 0
	}
	return int(n)
}

// --- Expressions ------------------------------------------------------------

// parseExpr parses a full expression with operator precedence, lowest first:
//
//	or          := and ('||' and)*
//	and         := comparison ('&&' comparison)*
//	comparison  := unary (('==' | '!=' | '<' | '<=' | '>' | '>=') unary)?   // non-chaining
//	unary       := '!' unary | primary
//	primary     := number | string | 'true' | 'false' | '(' expr ')' | ref | ref '(' args ')'
//
// A plain reference or call (the only expression forms before #199) is just a
// primary, so every earlier caller — assignment RHS, argument, return value,
// parallel branch — keeps working unchanged. The richer forms appear in `if`
// conditions and as call arguments.
func (p *parser) parseExpr() Expr { return p.parseOr() }

func (p *parser) parseOr() Expr {
	left := p.parseAnd()
	for left != nil && p.cur.Kind == KindOrOr {
		p.advance()
		right := p.parseAnd()
		left = &BinaryExpr{Pos: left.Position(), Op: KindOrOr, X: left, Y: right}
	}
	return left
}

func (p *parser) parseAnd() Expr {
	left := p.parseComparison()
	for left != nil && p.cur.Kind == KindAndAnd {
		p.advance()
		right := p.parseComparison()
		left = &BinaryExpr{Pos: left.Position(), Op: KindAndAnd, X: left, Y: right}
	}
	return left
}

func isComparisonOp(k Kind) bool {
	switch k {
	case KindEqEq, KindBangEq, KindLt, KindLte, KindGt, KindGte:
		return true
	}
	return false
}

func (p *parser) parseComparison() Expr {
	left := p.parseUnary()
	if left == nil || !isComparisonOp(p.cur.Kind) {
		return left
	}
	op := p.cur.Kind
	p.advance()
	right := p.parseUnary()
	expr := &BinaryExpr{Pos: left.Position(), Op: op, X: left, Y: right}
	if isComparisonOp(p.cur.Kind) {
		p.errorf(p.cur.Pos, "comparisons do not chain; parenthesize or use '&&' (got %s)", p.cur)
	}
	return expr
}

func (p *parser) parseUnary() Expr {
	if p.cur.Kind == KindBang {
		pos := p.cur.Pos
		p.advance()
		return &UnaryExpr{Pos: pos, Op: KindBang, X: p.parseUnary()}
	}
	return p.parsePrimary()
}

// parsePrimary parses a literal, a parenthesized expression, or a
// reference/call. `true` and `false` are recognized as boolean literals only in
// primary position and only when not immediately followed by `.` or `(`, so a
// (hypothetical) reference whose head is `true` is still reachable as a member
// access; the surface has no other use for those words.
func (p *parser) parsePrimary() Expr {
	switch p.cur.Kind {
	case KindNumber:
		return p.parseNumber()
	case KindString:
		lit := &LitExpr{Pos: p.cur.Pos, Kind: KindString, Value: p.cur.Lit}
		p.advance()
		return lit
	case KindLParen:
		p.advance()
		e := p.parseExpr()
		p.expect(KindRParen, "to close parenthesized expression")
		return e
	case KindLBrace:
		return p.parseObjectLiteral()
	case KindIdent:
		if (p.cur.Lit == "true" || p.cur.Lit == "false") && p.peekt.Kind != KindDot && p.peekt.Kind != KindLParen {
			lit := &LitExpr{Pos: p.cur.Pos, Kind: KindIdent, Value: p.cur.Lit == "true"}
			p.advance()
			return lit
		}
		ref := p.parseRef("in expression")
		if ref == nil {
			return nil
		}
		if p.cur.Kind == KindLParen {
			return &CallExpr{Pos: ref.Pos, Callee: ref, Args: p.parseArgs()}
		}
		return ref
	default:
		p.errorf(p.cur.Pos, "expected expression, got %s", p.cur)
		return nil
	}
}

// parseObjectLiteral parses `{ key: expr, key: expr }` — an object-literal expression (issue #440).
// Entries are separated by commas and/or newlines (both tolerated); a trailing comma is allowed. Keys
// are bare identifiers. The opening brace is the current token.
func (p *parser) parseObjectLiteral() Expr {
	obj := &ObjectExpr{Pos: p.cur.Pos}
	if _, ok := p.expect(KindLBrace, "to open an object literal"); !ok {
		return obj
	}
	for p.cur.Kind != KindRBrace && p.cur.Kind != KindEOF {
		if p.cur.Kind == KindComma {
			p.advance()
			continue
		}
		if p.cur.Kind != KindIdent {
			p.errorf(p.cur.Pos, "expected a field name in object literal, got %s", p.cur)
			p.syncLine()
			continue
		}
		fpos := p.cur.Pos
		key := p.ident("for an object-literal field")
		if _, ok := p.expect(KindColon, "after an object-literal field name"); !ok {
			p.syncLine()
			continue
		}
		val := p.parseExpr()
		obj.Fields = append(obj.Fields, &ObjectField{Pos: fpos, Key: key, Value: val})
	}
	p.expect(KindRBrace, "to close an object literal")
	return obj
}

// parseNumber converts the current KindNumber token to an int64 (no fractional
// part) or float64. A literal that overflows int64 falls back to float64.
func (p *parser) parseNumber() Expr {
	tok := p.cur
	p.advance()
	if !strings.Contains(tok.Lit, ".") {
		if i, err := strconv.ParseInt(tok.Lit, 10, 64); err == nil {
			return &LitExpr{Pos: tok.Pos, Kind: KindNumber, Value: i}
		}
	}
	f, err := strconv.ParseFloat(tok.Lit, 64)
	if err != nil {
		p.errorf(tok.Pos, "invalid number literal %q", tok.Lit)
		return &LitExpr{Pos: tok.Pos, Kind: KindNumber, Value: float64(0)}
	}
	return &LitExpr{Pos: tok.Pos, Kind: KindNumber, Value: f}
}

// parseRef parses a dotted reference path (pr, input.repo, github.get_pr).
func (p *parser) parseRef(context string) *RefExpr {
	parts := p.parseDottedPath(context)
	if len(parts) == 0 {
		return nil
	}
	return &RefExpr{Pos: parts[0].Pos, Parts: parts}
}

func (p *parser) parseArgs() []*Arg {
	if _, ok := p.expect(KindLParen, "to open argument list"); !ok {
		return nil
	}
	var args []*Arg
	for p.cur.Kind != KindRParen && p.cur.Kind != KindEOF {
		a := p.parseArg()
		if a == nil {
			p.syncList()
			continue
		}
		args = append(args, a)
		if p.cur.Kind == KindComma {
			p.advance()
			continue
		}
		break
	}
	p.expect(KindRParen, "to close argument list")
	return args
}

func (p *parser) parseArg() *Arg {
	// A named argument is `name: expr`; distinguished by the ':' lookahead so a
	// dotted reference (input.repo) is never mistaken for a name.
	if p.cur.Kind == KindIdent && p.peekt.Kind == KindColon {
		name := p.ident("argument name")
		p.advance() // ':'
		return &Arg{Pos: name.Pos, Name: name, Value: p.parseExpr()}
	}
	val := p.parseExpr()
	if val == nil {
		return nil
	}
	return &Arg{Pos: val.Position(), Value: val}
}

// --- Shared helpers ---------------------------------------------------------

// parseDottedPath parses IDENT ('.' IDENT)* and returns the segments. It
// returns nil when the leading identifier is missing (a diagnostic is recorded).
func (p *parser) parseDottedPath(context string) []*Ident {
	first := p.ident(context)
	if first == nil {
		return nil
	}
	parts := []*Ident{first}
	for p.cur.Kind == KindDot {
		p.advance()
		seg := p.ident("after '.'")
		if seg == nil {
			break
		}
		parts = append(parts, seg)
	}
	return parts
}

// dottedName joins identifier segments with '.' for diagnostics and effect names.
func dottedName(parts []*Ident) string {
	names := make([]string, len(parts))
	for i, p := range parts {
		names[i] = p.Name
	}
	return strings.Join(names, ".")
}
