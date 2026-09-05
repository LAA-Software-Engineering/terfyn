package lang

import (
	"sort"
	"strings"
)

// Comment preservation for `terfyn fmt` (issue #509).
//
// The printer emits fields in a fixed CANONICAL order, which is not the source order, so comments
// cannot be re-emitted by a "flush everything above the current print position" cursor — that would
// let a comment authored above one field glue itself to whichever field prints first, or leak out of
// its block entirely. Instead every comment is attached, up front, to a specific anchor by SOURCE
// position and brace scope, and emitted when that anchor prints — independent of print order:
//
//   - a trailing comment (same source line as code) attaches to that line;
//   - a standalone comment attaches as a LEADING comment of the next construct on a later line within
//     the SAME brace block (so it stays glued to what it documents even after canonical reordering);
//   - a standalone comment with no such following construct is a block TAIL comment, emitted at the
//     block's inner indent just before its closing brace, so it never escapes the block.
//
// A safety net (flushRemaining) emits anything an anchor never printed, so a comment is never dropped.
type commentIndex struct {
	texts    []string      // comment content by id, source order
	leading  map[int][]int // anchor line -> comment ids for precise inline placement before that line
	trailing map[int]int   // source line -> comment id for inline placement on a leaf line
	// byBlock maps a block's open line to EVERY comment inside that block, in source order. It is the
	// leak-proof backstop: a block's blockTail drains every one of its comments that a precise
	// leading/trailing hook did not already emit (as an own-line comment before the `}`), so no comment
	// can escape its block to file scope — regardless of which inner fields have print hooks
	// (issue #509 / PR #516 review). leading/trailing only refine WHERE inside the block a comment lands.
	byBlock    map[int][]int
	unattached []int // ids with no enclosing block (top-level, e.g. a footer) — flushed at the end
}

// buildCommentIndex attaches every comment to an anchor by source position and brace scope. src is the
// original source (for the brace scan); comments are the lexer's collected comments in source order.
func buildCommentIndex(src string, comments []Comment) *commentIndex {
	idx := &commentIndex{
		leading:  map[int][]int{},
		trailing: map[int]int{},
		byBlock:  map[int][]int{},
	}
	if len(comments) == 0 {
		return idx
	}
	idx.texts = make([]string, len(comments))
	for i, c := range comments {
		idx.texts[i] = c.Text
	}

	// Brace scan over the token stream (so braces inside strings/comments are ignored): the depth at
	// each code line's first token, the sorted anchor lines, and each block's open->close line span.
	lineDepth := map[int]int{}
	var anchorLines []int
	seen := map[int]bool{}
	type span struct{ open, close int }
	var spans []span
	var openStack []int
	depth := 0
	// Comment depth is the brace depth at the comment's position, and commentDecl is the KEYWORD line
	// of the top-level declaration a comment sits inside (0 when at file scope). Both are computed by
	// walking comments in lockstep with the tokens. commentDecl is the key `Print` passes to the
	// outermost blockTail, so registering an in-declaration comment under it guarantees a drain even
	// when a block's opening `{` is on a different line than its keyword (a split-brace workflow/agent
	// body), where the `{`-token-keyed span never matches (issue #509 / PR #516 review).
	commentDepth := make([]int, len(comments))
	commentDecl := make([]int, len(comments))
	curDecl := 0
	atDeclStart := true // the next depth-0 non-`}` token begins a new top-level declaration
	ck := 0
	assignCommentsBefore := func(pos Pos) {
		for ck < len(comments) && posBefore(comments[ck].Pos, pos) {
			commentDepth[ck] = depth
			commentDecl[ck] = curDecl
			ck++
		}
	}
	lx := NewLexer("", src)
	for {
		t := lx.Next()
		if t.Kind == KindEOF {
			break
		}
		assignCommentsBefore(t.Pos)
		if t.Kind == KindRBrace {
			if len(openStack) > 0 {
				open := openStack[len(openStack)-1]
				openStack = openStack[:len(openStack)-1]
				spans = append(spans, span{open: open, close: t.Pos.Line})
			}
			if depth > 0 {
				depth--
			}
			if depth == 0 {
				atDeclStart = true // this closed a top-level declaration; the next token starts a new one
			}
		}
		if depth == 0 && atDeclStart && t.Kind != KindRBrace {
			curDecl = t.Pos.Line
			atDeclStart = false
		}
		if !seen[t.Pos.Line] {
			seen[t.Pos.Line] = true
			lineDepth[t.Pos.Line] = depth
			anchorLines = append(anchorLines, t.Pos.Line)
		}
		if t.Kind == KindLBrace {
			openStack = append(openStack, t.Pos.Line)
			depth++
		}
	}
	for ; ck < len(comments); ck++ {
		commentDepth[ck] = depth
		commentDecl[ck] = curDecl
	}
	sort.Ints(anchorLines)

	// enclosing returns the innermost block span containing line L (the one with the largest open
	// below L whose close is at or after L). ok=false means top level (no enclosing block).
	enclosing := func(L int) (span, bool) {
		best := span{}
		found := false
		for _, s := range spans {
			if s.open < L && L <= s.close {
				if !found || s.open > best.open {
					best = s
					found = true
				}
			}
		}
		return best, found
	}

	for id, c := range comments {
		// Register the comment under EVERY enclosing block (not just the innermost), so an ancestor's
		// blockTail drains it even when an inner block's printer never calls blockTail. Every top-level
		// declaration calls blockTail, so this is leak-proof by construction: no comment can escape its
		// outermost enclosing declaration to file scope. The innermost block that does call blockTail
		// drains it first (inner blocks close before outer); `emitted` skips it in every ancestor.
		enclosed := false
		for _, s := range spans {
			if s.open < c.Pos.Line && c.Pos.Line <= s.close {
				idx.byBlock[s.open] = append(idx.byBlock[s.open], id)
				enclosed = true
			}
		}
		// Also register under the enclosing top-level declaration's keyword line — the key Print passes
		// to the outermost blockTail — so the drain fires even when a body's `{` is on its own line and
		// no span key matches what the printer passes (split-brace layouts). commentDecl is 0 at file
		// scope; a comment at depth 0 (between declarations, a header/footer) is left for flushRemaining.
		if commentDepth[id] > 0 && commentDecl[id] > 0 {
			idx.byBlock[commentDecl[id]] = append(idx.byBlock[commentDecl[id]], id)
			enclosed = true
		}
		if !enclosed {
			// No enclosing block: a top-level comment (e.g. a footer), left for flushRemaining.
			idx.unattached = append(idx.unattached, id)
		}

		if !c.Standalone {
			// Inline: refine placement onto its own line. Keep the first when two share a line (rare).
			if _, exists := idx.trailing[c.Pos.Line]; !exists {
				idx.trailing[c.Pos.Line] = id
			}
			continue
		}
		// Standalone: refine placement to lead the next construct on a later line, at the same depth,
		// still inside its innermost block. If found, a leadingBefore hook there emits it above that
		// construct; otherwise blockTail drains it. Either way byBlock guarantees emission in the block.
		d := commentDepth[id]
		closeLine := 1<<62 - 1
		if encl, ok := enclosing(c.Pos.Line); ok {
			closeLine = encl.close
		}
		for _, a := range anchorLines {
			if a <= c.Pos.Line {
				continue
			}
			if a >= closeLine {
				break
			}
			if lineDepth[a] == d {
				idx.leading[a] = append(idx.leading[a], id)
				break
			}
		}
	}
	return idx
}

// posBefore reports whether a precedes b in source order.
func posBefore(a, b Pos) bool {
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	return a.Column < b.Column
}

// printer is the formatter's output sink: a strings.Builder plus the comment attachment index and a
// per-id emitted flag, so each comment is written exactly once (issue #509).
type printer struct {
	strings.Builder
	idx     *commentIndex
	emitted []bool
}

func newPrinter(idx *commentIndex) *printer {
	if idx == nil {
		idx = &commentIndex{leading: map[int][]int{}, trailing: map[int]int{}, byBlock: map[int][]int{}}
	}
	return &printer{idx: idx, emitted: make([]bool, len(idx.texts))}
}

// leadingBefore emits the standalone comments attached as leading to `line`, each at the given indent.
func (p *printer) leadingBefore(line int, indent string) {
	for _, id := range p.idx.leading[line] {
		p.emitComment(id, indent)
	}
}

// blockTail drains, before a block's closing brace and at the block's inner indent, every comment
// inside that block that a precise leading/trailing hook did not already emit — so no comment can
// escape its block to file scope, whatever inner fields lack hooks. Each emit is guarded by `emitted`,
// so comments already placed inline are not repeated; the rest degrade to own-line comments here.
func (p *printer) blockTail(openLine int, indent string) {
	for _, id := range p.idx.byBlock[openLine] {
		p.emitComment(id, indent)
	}
}

// trailingOn attaches the inline comment for `line` to the line just written, before its newline.
func (p *printer) trailingOn(line int) {
	id, ok := p.idx.trailing[line]
	if !ok || p.emitted[id] {
		return
	}
	p.emitted[id] = true
	p.WriteString(" //")
	if t := p.idx.texts[id]; t != "" {
		p.WriteString(" ")
		p.WriteString(t)
	}
}

// flushRemaining emits any comment no anchor printed (unattached, or an anchor line that was never
// reached), so nothing is ever dropped. Called once at the end of Print.
func (p *printer) flushRemaining(indent string) {
	for id := range p.emitted {
		if !p.emitted[id] {
			p.emitComment(id, indent)
		}
	}
}

func (p *printer) emitComment(id int, indent string) {
	if p.emitted[id] {
		return
	}
	p.emitted[id] = true
	p.WriteString(indent)
	p.WriteString("//")
	if t := p.idx.texts[id]; t != "" {
		p.WriteString(" ")
		p.WriteString(t)
	}
	p.WriteString("\n")
}

// field writes a single-line construct — `indent+text` — then attaches a trailing comment for srcLine
// if one is pending, then the newline. Emit leading comments with leadingBefore before calling this.
func (p *printer) field(indent, text string, srcLine int) {
	p.WriteString(indent)
	p.WriteString(text)
	p.trailingOn(srcLine)
	p.WriteString("\n")
}
