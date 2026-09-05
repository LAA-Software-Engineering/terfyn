package lang

import (
	"strings"
	"unicode/utf8"
)

// Lexer scans .agent source into a token stream. It is newline-insensitive:
// statement and field boundaries are recovered by the grammar (each construct
// has a deterministic shape), so newlines are ordinary whitespace and are not
// emitted as tokens. Line comments (// to end of line) are not emitted as tokens
// either, but their text and position are collected (see Comments) so the
// formatter can round-trip them (issue #509).
type Lexer struct {
	src  string
	file string

	// offset is the byte index of the next rune to read.
	offset int
	// line and col are the 1-based position of the rune at offset.
	line, col int

	diags Diagnostics

	// comments holds every // line comment in source order, classified standalone
	// vs trailing (see Comment) by whether anything precedes it on its line.
	comments []Comment
}

// Comments returns the // line comments recovered in source order (issue #509).
func (l *Lexer) Comments() []Comment { return l.comments }

// Comment is one // line comment recovered by the lexer for the formatter (issue #509).
// Text is the content after "//" with surrounding whitespace trimmed (no leading "//",
// no trailing newline). Standalone is true when the comment sits on its own line; false
// when it trails code on the same line (e.g. `model x // note`).
type Comment struct {
	Pos        Pos
	Text       string
	Standalone bool
}

// NewLexer returns a lexer over src. file is recorded in every token's Pos.
func NewLexer(file, src string) *Lexer {
	return &Lexer{src: src, file: file, offset: 0, line: 1, col: 1}
}

// Diagnostics returns any lexical errors accumulated so far (stray runes).
func (l *Lexer) Diagnostics() Diagnostics { return l.diags }

// pos returns the current position.
func (l *Lexer) pos() Pos { return Pos{File: l.file, Line: l.line, Column: l.col} }

// peek returns the rune at offset without consuming it, and its byte width.
// It returns (utf8.RuneError, 0) at end of input.
func (l *Lexer) peek() (rune, int) {
	if l.offset >= len(l.src) {
		return utf8.RuneError, 0
	}
	r, w := utf8.DecodeRuneInString(l.src[l.offset:])
	return r, w
}

// advance consumes one rune, updating offset, line, and column.
func (l *Lexer) advance() rune {
	r, w := l.peek()
	if w == 0 {
		return utf8.RuneError
	}
	l.offset += w
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

// Next returns the next token. After end of input it returns KindEOF
// repeatedly.
func (l *Lexer) Next() Token {
	l.skipTrivia()
	start := l.pos()
	r, w := l.peek()
	if w == 0 {
		return Token{Kind: KindEOF, Pos: start}
	}

	switch {
	case isIdentStart(r):
		return l.scanIdent(start)
	case isDigit(r):
		return l.scanNumber(start)
	case r == '"':
		if strings.HasPrefix(l.src[l.offset:], `"""`) {
			return l.scanMultilineString(start)
		}
		return l.scanString(start)
	case r == '{':
		l.advance()
		return Token{Kind: KindLBrace, Lit: "{", Pos: start}
	case r == '}':
		l.advance()
		return Token{Kind: KindRBrace, Lit: "}", Pos: start}
	case r == '(':
		l.advance()
		return Token{Kind: KindLParen, Lit: "(", Pos: start}
	case r == ')':
		l.advance()
		return Token{Kind: KindRParen, Lit: ")", Pos: start}
	case r == '.':
		l.advance()
		return Token{Kind: KindDot, Lit: ".", Pos: start}
	case r == '/':
		l.advance()
		return Token{Kind: KindSlash, Lit: "/", Pos: start}
	case r == ',':
		l.advance()
		return Token{Kind: KindComma, Lit: ",", Pos: start}
	case r == ':':
		l.advance()
		return Token{Kind: KindColon, Lit: ":", Pos: start}
	case r == '=':
		l.advance()
		if next, nw := l.peek(); nw != 0 && next == '=' {
			l.advance()
			return Token{Kind: KindEqEq, Lit: "==", Pos: start}
		}
		return Token{Kind: KindEquals, Lit: "=", Pos: start}
	case r == '!':
		l.advance()
		if next, nw := l.peek(); nw != 0 && next == '=' {
			l.advance()
			return Token{Kind: KindBangEq, Lit: "!=", Pos: start}
		}
		return Token{Kind: KindBang, Lit: "!", Pos: start}
	case r == '<':
		l.advance()
		if next, nw := l.peek(); nw != 0 && next == '=' {
			l.advance()
			return Token{Kind: KindLte, Lit: "<=", Pos: start}
		}
		return Token{Kind: KindLt, Lit: "<", Pos: start}
	case r == '>':
		l.advance()
		if next, nw := l.peek(); nw != 0 && next == '=' {
			l.advance()
			return Token{Kind: KindGte, Lit: ">=", Pos: start}
		}
		return Token{Kind: KindGt, Lit: ">", Pos: start}
	case r == '&':
		l.advance()
		if next, nw := l.peek(); nw != 0 && next == '&' {
			l.advance()
			return Token{Kind: KindAndAnd, Lit: "&&", Pos: start}
		}
		l.errorf(start, "unexpected %q (did you mean '&&'?)", "&")
		return Token{Kind: KindError, Lit: "&", Pos: start}
	case r == '|':
		l.advance()
		if next, nw := l.peek(); nw != 0 && next == '|' {
			l.advance()
			return Token{Kind: KindOrOr, Lit: "||", Pos: start}
		}
		l.errorf(start, "unexpected %q (did you mean '||'?)", "|")
		return Token{Kind: KindError, Lit: "|", Pos: start}
	case r == '-':
		// The only multi-rune operator: -> . A lone '-' is not an identifier
		// start (isIdentStart excludes it) and is a lexer error.
		l.advance()
		if next, nw := l.peek(); nw != 0 && next == '>' {
			l.advance()
			return Token{Kind: KindArrow, Lit: "->", Pos: start}
		}
		l.errorf(start, "unexpected %q (did you mean '->'?)", "-")
		return Token{Kind: KindError, Lit: "-", Pos: start}
	default:
		l.advance()
		l.errorf(start, "unexpected character %q", string(r))
		return Token{Kind: KindError, Lit: string(r), Pos: start}
	}
}

// scanIdent reads an identifier starting at start and classifies it as a
// keyword or KindIdent.
func (l *Lexer) scanIdent(start Pos) Token {
	begin := l.offset
	l.advance() // first rune (already validated as an ident start)
	for {
		r, w := l.peek()
		if w == 0 || !isIdentPart(r) {
			break
		}
		l.advance()
	}
	lit := l.src[begin:l.offset]
	if kind, ok := keywords[lit]; ok {
		return Token{Kind: kind, Lit: lit, Pos: start}
	}
	return Token{Kind: KindIdent, Lit: lit, Pos: start}
}

// scanNumber reads an integer or decimal literal: [0-9]+ ('.' [0-9]+)?. A '.'
// is consumed into the number only when a digit follows it, so `steps.0` (were
// it ever written) keeps the dot as its own token. The raw text is carried in
// Token.Lit; the parser converts it to an int64 or float64. The language has no
// arithmetic, so numbers appear only as condition operands and call arguments.
func (l *Lexer) scanNumber(start Pos) Token {
	begin := l.offset
	for {
		r, w := l.peek()
		if w == 0 || !isDigit(r) {
			break
		}
		l.advance()
	}
	// Fractional part only if a digit follows the '.'.
	if r, w := l.peek(); w != 0 && r == '.' && l.offset+1 < len(l.src) && isDigit(rune(l.src[l.offset+1])) {
		l.advance() // '.'
		for {
			r, w := l.peek()
			if w == 0 || !isDigit(r) {
				break
			}
			l.advance()
		}
	}
	return Token{Kind: KindNumber, Lit: l.src[begin:l.offset], Pos: start}
}

// scanString reads a double-quoted string literal with the escapes \" \\ \n \t
// \r. Token.Lit holds the DECODED value. An unterminated string or an unknown
// escape is a lexer diagnostic; the best-effort decoded prefix is still
// returned so the parser can continue.
func (l *Lexer) scanString(start Pos) Token {
	l.advance() // opening quote
	var b strings.Builder
	for {
		r, w := l.peek()
		if w == 0 {
			l.errorf(start, "unterminated string literal")
			return Token{Kind: KindString, Lit: b.String(), Pos: start}
		}
		if r == '"' {
			l.advance()
			return Token{Kind: KindString, Lit: b.String(), Pos: start}
		}
		if r == '\n' {
			l.errorf(start, "unterminated string literal (newline before closing quote)")
			return Token{Kind: KindString, Lit: b.String(), Pos: start}
		}
		if r == '\\' {
			l.advance()
			esc, ew := l.peek()
			if ew == 0 {
				continue
			}
			switch esc {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				l.errorf(l.pos(), "unknown escape %q in string literal", "\\"+string(esc))
				b.WriteRune(esc)
			}
			l.advance()
			continue
		}
		b.WriteRune(r)
		l.advance()
	}
}

// scanMultilineString reads a triple-quoted string literal `"""…"""`. The body
// is RAW — no escape processing and no `${…}` interpolation — so real prompts can
// contain backslashes and braces literally. Token.Lit holds the value after
// deterministic indentation normalization (see normalizeMultiline). An
// unterminated literal is a diagnostic; the best-effort decoded prefix is still
// returned so the parser can continue.
func (l *Lexer) scanMultilineString(start Pos) Token {
	l.advance() // "
	l.advance() // "
	l.advance() // "  (opening delimiter consumed)
	begin := l.offset
	for {
		if l.offset >= len(l.src) {
			l.errorf(start, "unterminated multiline string literal")
			return Token{Kind: KindString, Lit: normalizeMultiline(l.src[begin:l.offset]), Pos: start}
		}
		if strings.HasPrefix(l.src[l.offset:], `"""`) {
			body := l.src[begin:l.offset]
			l.advance()
			l.advance()
			l.advance() // closing delimiter consumed
			return Token{Kind: KindString, Lit: normalizeMultiline(body), Pos: start}
		}
		l.advance()
	}
}

// normalizeMultiline applies the deterministic indentation rules for a
// triple-quoted literal (docs/LANGUAGE.md): line endings are normalized to \n; a
// whitespace-only opening line (the newline right after `"""`) is discarded; a
// whitespace-only closing line (the indentation before the closing `"""`) is
// discarded; the common leading indentation of all nonblank lines is removed,
// preserving relative indentation; and blank lines are preserved as empty lines.
func normalizeMultiline(raw string) string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	// Discard a leading opening line that is only whitespace up to its newline
	// (`"""` immediately — or after trailing spaces — followed by a newline).
	if i := strings.IndexByte(raw, '\n'); i >= 0 && strings.TrimSpace(raw[:i]) == "" {
		raw = raw[i+1:]
	}
	lines := strings.Split(raw, "\n")
	// A trailing whitespace-only line is the closing delimiter's own indented
	// line: drop it together with the newline that preceded it.
	if n := len(lines); n > 0 && strings.TrimRight(lines[n-1], " \t") == "" {
		lines = lines[:n-1]
	}
	// Common leading indentation over nonblank lines only.
	minIndent := -1
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		n := len(ln) - len(strings.TrimLeft(ln, " \t"))
		if minIndent < 0 || n < minIndent {
			minIndent = n
		}
	}
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			lines[i] = "" // preserve the blank line, drop incidental whitespace
			continue
		}
		if minIndent > 0 && len(ln) >= minIndent {
			lines[i] = ln[minIndent:]
		}
	}
	return strings.Join(lines, "\n")
}

// skipTrivia consumes whitespace (including newlines) and // line comments.
func (l *Lexer) skipTrivia() {
	for {
		r, w := l.peek()
		if w == 0 {
			return
		}
		switch {
		case r == ' ' || r == '\t' || r == '\r' || r == '\n':
			l.advance()
		case r == '/':
			// Look ahead for a second '/'. A single slash is a real token.
			if l.offset+1 < len(l.src) && l.src[l.offset+1] == '/' {
				l.collectLineComment()
				continue
			}
			return
		default:
			return
		}
	}
}

// collectLineComment consumes a // comment through the end of the line (the
// terminating newline is left for skipTrivia to count) and records it for the
// formatter (issue #509). The comment is standalone when only whitespace precedes
// it on its line, otherwise it trails the code on that line. Text is the content
// after "//" with surrounding whitespace trimmed.
func (l *Lexer) collectLineComment() {
	start := l.pos()
	startOff := l.offset
	standalone := l.onlyBlankBeforeLineStart(startOff)
	l.advance() // first '/'
	l.advance() // second '/'
	begin := l.offset
	for {
		r, w := l.peek()
		if w == 0 || r == '\n' {
			break
		}
		l.advance()
	}
	l.comments = append(l.comments, Comment{
		Pos:        start,
		Text:       strings.TrimSpace(l.src[begin:l.offset]),
		Standalone: standalone,
	})
}

// onlyBlankBeforeLineStart reports whether every byte from the start of off's line
// up to off is a space or tab — i.e. the token at off is the first non-blank on its
// line. Used to classify a comment as standalone (own line) vs trailing.
func (l *Lexer) onlyBlankBeforeLineStart(off int) bool {
	for i := off - 1; i >= 0; i-- {
		c := l.src[i]
		if c == '\n' {
			return true
		}
		if c != ' ' && c != '\t' && c != '\r' {
			return false
		}
	}
	return true
}

func (l *Lexer) errorf(pos Pos, format string, args ...any) {
	l.diags = append(l.diags, diagf(pos, format, args...))
}

// isIdentStart reports whether r may begin an identifier.
func isIdentStart(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// isIdentPart reports whether r may continue an identifier. Hyphens and digits
// are allowed after the first rune so guarded-writes and gpt-5 lex as one token.
func isIdentPart(r rune) bool {
	return isIdentStart(r) || (r >= '0' && r <= '9') || r == '-'
}

// isDigit reports whether r is an ASCII decimal digit.
func isDigit(r rune) bool { return r >= '0' && r <= '9' }
