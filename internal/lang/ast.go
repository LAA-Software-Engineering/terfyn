package lang

// The typed AST for the .agent surface (ADR 002). Every node implements Node
// and carries a spec.Pos (aliased as Pos) at its start. The tree records the
// declarations as written, not what they resolve to — reference resolution,
// typing, and effect checking are #198, and lowering to the resource model is
// #197. A construct the grammar admits at most once (an agent field) is
// reported as a diagnostic if repeated rather than silently overwritten, so no
// written declaration is dropped without a diagnostic.

// Node is any AST node. Position returns the node's start position.
type Node interface {
	Position() Pos
}

// File is the root: an ordered list of top-level declarations.
type File struct {
	Pos   Pos
	Decls []Decl
	// Comments are the // line comments recovered by the lexer in source order, so the
	// formatter can round-trip them (issue #509). They are not part of the resource model.
	Comments []Comment
	// cidx attaches each comment to a print anchor by source position; built by Parse (which has the
	// source), consumed by Print. nil on an AST built without source (e.g. raise/migrate) — then Print
	// emits no comments, unchanged.
	cidx *commentIndex
}

func (f *File) Position() Pos { return f.Pos }

// Decl is a top-level declaration: an AgentDecl or a WorkflowDecl.
type Decl interface {
	Node
	declNode()
}

// Ident is a bare identifier occurrence with its position.
type Ident struct {
	Pos  Pos
	Name string
}

func (i *Ident) Position() Pos { return i.Pos }

// --- Agent declarations -----------------------------------------------------

// AgentDecl is `agent <Name> { ... }`. Each field appears at most once; a field
// the author omitted is a nil pointer (requiredness is a checking concern, #198)
// and a repeated field keeps the first occurrence and yields a duplicate-field
// diagnostic (the grammar admits each field once). Fields do not preserve
// source order across kinds — the surface fixes their meaning by keyword, not
// position.
type AgentDecl struct {
	Pos          Pos
	Name         *Ident
	Model        *ModelRef  // model <provider>/<name>
	Policy       *Ident     // policy <name> (reference to a Policy resource)
	Description  *StringLit // description "..." (lowers to AgentSpec.Description)
	Instructions *StringLit // instructions "..." (inline prompt; lowers to AgentSpec.Instructions)
	// InstructionsFile is the `instructions file("path")` form (#360): a load-time file reference,
	// mutually exclusive with Instructions. Its UTF-8 contents lower verbatim into
	// AgentSpec.Instructions exactly as an inline string would.
	InstructionsFile *InstructionsFile
	Constraints      *Constraints // constraints { maxIterations ... } (lowers to AgentSpec.Constraints)
	Grants           []*Grant     // grants { tool.<name>.<operation> ... }
	GrantsPos        Pos          // position of the `grants` keyword (formatter comment anchoring, #509)
	Input            *TypeRef     // input <Type>
	Output           *TypeRef     // output <Type>
}

// Constraints is the `constraints { ... }` block: the fixed set of agent execution
// bounds (#310) that lowers to spec.AgentConstraints. A field pointer is nil when
// the author omitted it; each field appears at most once.
type Constraints struct {
	Pos                     Pos
	MaxIterations           *int
	MaxTokens               *int
	TimeoutSeconds          *int
	Temperature             *float64
	RequireStructuredOutput *bool
}

func (c *Constraints) Position() Pos { return c.Pos }

func (d *AgentDecl) Position() Pos { return d.Pos }
func (d *AgentDecl) declNode()     {}

// StringLit is a string literal occurrence with its position and decoded value.
// It carries the same value whether the source used a single-quoted `"..."` or a
// triple-quoted `"""..."""` literal (the multiline form is an ordinary string, not
// a distinct AST type); the lexer has already normalized a multiline body.
type StringLit struct {
	Pos   Pos
	Value string
}

func (s *StringLit) Position() Pos { return s.Pos }

// InstructionsFile is `instructions file("path")` — a load-time file reference (#360). Path is the
// quoted relative path literal; Resolved holds the file's UTF-8 contents once the project loader has
// read and validated the path. It is a pointer so an unresolved reference (nil, e.g. after a bare
// lang.Parse under `terfyn fmt`, which re-emits the file("...") form) is distinct from a
// legitimately empty file (non-nil ""): lowering rejects an unresolved ref rather than silently
// pinning an empty prompt into the spec hash / deployment snapshot. The resolved text lowers
// verbatim into AgentSpec.Instructions like any inline instruction — a changed file surfaces as a
// plan diff, not a silent behavior change.
type InstructionsFile struct {
	Pos      Pos
	Path     *StringLit
	Resolved *string
}

func (f *InstructionsFile) Position() Pos { return f.Pos }

// ModelRef is a `<provider>/<name>` model reference such as openai/gpt-5.
// Provider and Name preserve hyphens (gpt-5); Raw is the reassembled text.
type ModelRef struct {
	Pos      Pos
	Provider string
	Name     string
	Raw      string
}

func (m *ModelRef) Position() Pos { return m.Pos }

// Grant is one autonomous capability bound inside a `grants { }` block. Per the
// ADR 002 amendment (#188) a grant names a concrete operation as
// tool.<Name>.<Operation> — the exact reference vocabulary of
// approvals.requiredFor / uses: — and lives in a namespace distinct from
// effects. It is split the same way as tools.ParseUses: the leading "tool"
// segment is the namespace marker, Name is the single tool-name segment, and
// Operation is everything after it. Operation is therefore a dotted path, not a
// single identifier: shipped strings such as tool.github.pull_request.get carry
// a multi-segment operation (pull_request.get), and a lowering pass (#197)
// reconstructs the uses string as tool.<Name>.<Operation joined by ".">.
// A grant that omits the tool. prefix is a diagnostic, not an EffectRef.
type Grant struct {
	Pos  Pos
	Name *Ident // the <Name> segment (tool.<Name>.<operation>); a single identifier
	// Operation is the operation path (the segments after the tool name), at
	// least one identifier and possibly dotted (pull_request.post_comment).
	Operation []*Ident
	// Segments is the full dotted path as written (including the leading
	// "tool"), preserved for diagnostics and round-tripping.
	Segments []*Ident
}

func (g *Grant) Position() Pos { return g.Pos }

// ToolName returns the granted tool's name, or "" if the grant is malformed.
func (g *Grant) ToolName() string {
	if g.Name == nil {
		return ""
	}
	return g.Name.Name
}

// OperationName returns the dotted operation path (e.g. "pull_request.get"), or
// "" if the grant is malformed. This is the <operation> half that ParseUses
// yields and that a uses string reconstructs after the tool name.
func (g *Grant) OperationName() string { return dottedName(g.Operation) }

// --- Workflow declarations --------------------------------------------------

// WorkflowDecl is `workflow <Name>(<params>) -> <Result> effects { ... } { body }`.
// Result and the effects clause are optional in the grammar; the body is a
// statement list that may include conditionals and loops (IfStmt, ForStmt; #199)
// in addition to assignments, calls, parallel blocks, and a return.
type WorkflowDecl struct {
	Pos         Pos
	Name        *Ident
	Params      []*Param
	Result      *TypeRef     // return type after ->; nil if omitted
	Description *StringLit   // description "..."; nil if omitted (lowers to WorkflowSpec.Description)
	Policy      *Ident       // policy <name>; nil if omitted (lowers to WorkflowSpec.Policy)
	Effects     []*EffectRef // effects { github.read, ... }; nil if no clause
	Body        []Stmt
}

func (d *WorkflowDecl) Position() Pos { return d.Pos }
func (d *WorkflowDecl) declNode()     {}

// Param is one `<name>: <Type>` workflow parameter.
type Param struct {
	Pos  Pos
	Name *Ident
	Type *TypeRef
}

func (p *Param) Position() Pos { return p.Pos }

// TypeRef names a schema/type by identifier (e.g. PullRequest, Review). It is an
// unresolved reference at the parse layer.
type TypeRef struct {
	Pos  Pos
	Name string
}

func (t *TypeRef) Position() Pos { return t.Pos }

// EffectRef is one bare dotted effect identifier in the effects clause, such as
// github.read or external.visible. Unlike a Grant it carries no tool. prefix;
// the two namespaces must never be interchangeable (ADR 002).
type EffectRef struct {
	Pos  Pos
	Name string // dotted, e.g. "github.read"
}

func (e *EffectRef) Position() Pos { return e.Pos }

// --- Statements -------------------------------------------------------------

// Stmt is a workflow body statement.
type Stmt interface {
	Node
	stmtNode()
}

// AssignStmt is `<Target> = <Value>` binding a name to an expression result.
type AssignStmt struct {
	Pos    Pos
	Target *Ident
	Value  Expr
}

func (s *AssignStmt) Position() Pos { return s.Pos }
func (s *AssignStmt) stmtNode()     {}

// ExprStmt is a bare expression used for its effect, e.g. a deterministic tool
// call whose result is unbound: github.post_comment(...).
type ExprStmt struct {
	Pos Pos
	X   Expr
}

func (s *ExprStmt) Position() Pos { return s.Pos }
func (s *ExprStmt) stmtNode()     {}

// ParallelStmt is `parallel { <AssignStmt>... }` — static fan-out into named
// branches with fan-in (ADR 002 graph structure; #192). Each branch binds a
// name, so the body admits only assignments.
type ParallelStmt struct {
	Pos  Pos
	Body []*AssignStmt
}

func (s *ParallelStmt) Position() Pos { return s.Pos }
func (s *ParallelStmt) stmtNode()     {}

// ReturnStmt is `return <Value>`.
type ReturnStmt struct {
	Pos   Pos
	Value Expr
}

func (s *ReturnStmt) Position() Pos { return s.Pos }
func (s *ReturnStmt) stmtNode()     {}

// ApprovalStmt is `approval <Bind> { description "…" redactKeys { "k1" "k2" } }` (#440): a workflow
// graph-node human pause, the `.agent` form of a YAML `approval:` step. It lowers to an
// [execir.Approval] node (execution) and to a spec.WorkflowStep with an Approval value (the resource
// projection effect analysis walks). Bind names the decision; Description and RedactKeys are optional.
type ApprovalStmt struct {
	Pos         Pos
	Bind        *Ident
	Description *StringLit
	RedactKeys  []*StringLit
	// With is the review payload (named args), the same load-bearing data a YAML approval step's `with:`
	// carries: it is shown to the reviewer, gates the Edit decision (offered only when non-empty), and
	// becomes the node's published output. Lowered through the same Args path as a call's arguments.
	With []*Arg
}

func (s *ApprovalStmt) Position() Pos { return s.Pos }
func (s *ApprovalStmt) stmtNode()     {}

// IfStmt is `if <Cond> { <Then> } (else ({ <Else> } | <IfStmt>))?` (#199). Cond
// is a boolean expression; Then and Else are statement lists. An `else if` chain
// parses as an Else holding a single nested IfStmt. Control flow never becomes a
// field on the resource-model WorkflowStep (ADR 002 §4): it lowers to the
// execution IR's Branch, and its two arms both flatten into the resource
// projection so the effect bound is the union over branches (ADR 002 §5).
type IfStmt struct {
	Pos  Pos
	Cond Expr
	Then []Stmt
	Else []Stmt
}

func (s *IfStmt) Position() Pos { return s.Pos }
func (s *IfStmt) stmtNode()     {}

// ForStmt is `for <Var> in <In> { <Body> }` (#199): iteration over a runtime
// collection. Parallel marks the dynamic fan-out form `parallel for <Var> in
// <In> { }` — ADR 002 §1 classifies dynamic fan-out over a runtime collection as
// "a loop wearing a graph costume," so it is language work, not a graph field.
// Both forms lower to the execution IR's Loop; only Parallel runs its iterations
// with bounded concurrency. Var binds inside Body only.
type ForStmt struct {
	Pos      Pos
	Var      *Ident
	In       Expr
	Body     []Stmt
	Parallel bool
}

func (s *ForStmt) Position() Pos { return s.Pos }
func (s *ForStmt) stmtNode()     {}

// WhileStmt is `while <Cond> limit <N> { <Body> }` (#288): a bounded loop that
// runs its body while Cond is truthy, at most Limit times. The bound is part of
// the safety model — Terfyn admits no unbounded effectful loop (ADR 002 §6) — so
// Limit is always a positive integer literal fixed in the source (a missing,
// zero, fractional, or dynamic bound is a diagnostic; the parser records 0 in
// that case). Scoping is the sequential-loop rule (loop-carried state): a name
// bound before the loop may be rebound and carries forward across iterations and
// out of the loop; a name first bound inside is loop-local. It lowers to the
// execution IR's While (never a WorkflowStep field, ADR 002 §4).
type WhileStmt struct {
	Pos   Pos
	Cond  Expr
	Limit int
	Body  []Stmt
}

func (s *WhileStmt) Position() Pos { return s.Pos }
func (s *WhileStmt) stmtNode()     {}

// RetryStmt is `retry until <Cond> limit <N> { <Body> }` (#361): a bounded RETRY loop,
// the fail-on-exhaustion companion to `while`. It runs its body until Cond (the SUCCESS
// condition) becomes truthy, at most Limit times; the condition is checked AFTER each
// attempt so the body always runs at least once, and — unlike `while`, which exits
// silently when its bound is reached — if the attempts are exhausted with Cond still
// false the run FAILS with an explicit, deterministic outcome rather than falling through.
// Limit is a positive integer literal (the same bound rule as `while`; ADR 002 §6), so the
// per-run iteration bound it contributes to `terfyn plan` is identical. Loop-carried state
// follows the sequential-loop rule. It lowers to the execution IR's Retry.
type RetryStmt struct {
	Pos   Pos
	Cond  Expr
	Limit int
	Body  []Stmt
}

func (s *RetryStmt) Position() Pos { return s.Pos }
func (s *RetryStmt) stmtNode()     {}

// --- Expressions ------------------------------------------------------------

// Expr is a workflow expression: a CallExpr, a RefExpr, or — in a condition or
// call argument (#199) — a LitExpr, UnaryExpr, or BinaryExpr.
type Expr interface {
	Node
	exprNode()
}

// LitExpr is a literal operand: a string, a number, or a boolean (#199). Kind is
// one of KindString, KindNumber, or a boolean (recorded as KindIdent with a
// bool Value). Value holds the decoded Go value: string, int64, float64, or
// bool. Literals appear in conditions and as call arguments; the surface has no
// arithmetic, so numbers are only ever compared or passed, never combined.
type LitExpr struct {
	Pos   Pos
	Kind  Kind
	Value any
}

func (e *LitExpr) Position() Pos { return e.Pos }
func (e *LitExpr) exprNode()     {}

// UnaryExpr is `<Op> <X>` — only `!` (logical negation) exists in the surface.
type UnaryExpr struct {
	Pos Pos
	Op  Kind // KindBang
	X   Expr
}

func (e *UnaryExpr) Position() Pos { return e.Pos }
func (e *UnaryExpr) exprNode()     {}

// BinaryExpr is `<X> <Op> <Y>`: a comparison (== != < <= > >=) or a logical
// connective (&& ||). Comparisons do not chain (a < b < c is a syntax error);
// logical connectives are left-associative with && binding tighter than ||.
type BinaryExpr struct {
	Pos Pos
	Op  Kind
	X   Expr
	Y   Expr
}

func (e *BinaryExpr) Position() Pos { return e.Pos }
func (e *BinaryExpr) exprNode()     {}

// RefExpr is a dotted reference path: a bare name (pr), a member access
// (input.repo, result.summary), or a callee path (github.get_pr). Parts holds
// each dotted segment in order and is always non-empty.
type RefExpr struct {
	Pos   Pos
	Parts []*Ident
}

func (e *RefExpr) Position() Pos { return e.Pos }
func (e *RefExpr) exprNode()     {}

// CallExpr is `<Callee>(<args>)`. Callee is the dotted reference being invoked
// (a workflow-level tool call like github.get_pr, or an agent/subworkflow
// invocation like SecurityReviewer). Args may nest arbitrarily.
type CallExpr struct {
	Pos    Pos
	Callee *RefExpr
	Args   []*Arg
}

func (e *CallExpr) Position() Pos { return e.Pos }
func (e *CallExpr) exprNode()     {}

// Arg is one call argument. Name is nil for a positional argument and set for a
// named one (repo: input.repo). A single call may mix positional and named
// arguments at the parse layer; validity is a checking concern (#198).
type Arg struct {
	Pos   Pos
	Name  *Ident // nil => positional
	Value Expr
}

func (a *Arg) Position() Pos { return a.Pos }

// ObjectExpr is a `{ key: expr, key: expr }` object literal (issue #440). It is the multi-field
// counterpart to a scalar `return <expr>`: `return { a: x, b: y }` produces a workflow output whose
// value map is `{a: …, b: …}` directly (matching a YAML `output.value: {a, b}`), rather than the
// single-`value` envelope a scalar return uses. Field values are arbitrary expressions.
type ObjectExpr struct {
	Pos    Pos
	Fields []*ObjectField
}

func (e *ObjectExpr) Position() Pos { return e.Pos }
func (e *ObjectExpr) exprNode()     {}

// ObjectField is one `key: value` entry in an ObjectExpr. Key is a bare identifier.
type ObjectField struct {
	Pos   Pos
	Key   *Ident
	Value Expr
}

func (f *ObjectField) Position() Pos { return f.Pos }
