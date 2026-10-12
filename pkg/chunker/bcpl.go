package chunker

import (
	"bytes"
	"regexp"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

// BCPL comes in two dialects that the grammar does not tell apart. The one
// that is compiled today writes its reserved words in upper case, tells
// upper from lower case in names and brackets its sections with { and }.
// The one of the 1980s, of TRIPOS and AmigaDOS, tells no cases apart
// anywhere (let, Let and LET are one word, and so are cgerror and CGERROR),
// brackets with $( and $), and has a conditional compilation of its own:
//
//	$$TAG := TRUE              sets a tag
//	$<TAG ... $>TAG            is compiled when the tag is set
//	$<TAG' ... $>TAG'          is compiled when it is not
//
// The grammar is that of the first. Source in the second is brought to it
// before it is parsed, byte for byte, so that every place in the tree is
// still that place in the file.

// bcplReserved are the reserved words of the old dialect: the list of
// declsyswords in the TRIPOS compiler. What the language reserved later
// (MOD, XOR, NEXT, EXIT, SKIP, ...) is a name there.
var bcplReserved = setOf(
	"AND", "ABS", "BE", "BREAK", "BY", "CASE", "DO", "DEFAULT", "EQ", "EQV", "ELSE", "ENDCASE", "FALSE", "FOR",
	"FINISH", "GOTO", "GE", "GR", "GLOBAL", "GET", "IF", "INTO", "LET", "LV", "LE", "LS", "LOGOR", "LOGAND",
	"LOOP", "LSHIFT", "MANIFEST", "NE", "NOT", "NEQV", "NEEDS", "OF", "OR", "RESULTIS", "RETURN", "REM",
	"RSHIFT", "RV", "REPEAT", "REPEATWHILE", "REPEATUNTIL", "SWITCHON", "SLCT", "STATIC", "SECTION", "TO",
	"TEST", "TRUE", "THEN", "TABLE", "UNTIL", "UNLESS", "VEC", "VALOF", "WHILE",
)

// span is a range of bytes
type span struct{ start, end int }

// bcplText is a BCPL source as the parser is to see it
type bcplText struct {
	text []byte
	old  bool   // The dialect that tells no cases apart
	arms []span // Conditionally compiled text, with the tags around it
	// A block comment has the start of another in it, so that it matters
	// whether they nest
	nesting bool
}

func isBCPLNameStart(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func isBCPLName(c byte) bool {
	return isBCPLNameStart(c) || c >= '0' && c <= '9' || c == '.' || c == '_'
}

// blank overwrites a range with spaces, keeping the line breaks
func blank(text []byte, start, end int) {
	for i := start; i < end; i++ {
		if text[i] != '\n' && text[i] != '\r' {
			text[i] = ' '
		}
	}
}

// bcplRead reads a BCPL source for what the grammar does not know of the old
// dialect, and returns a copy of it without that: tag settings blanked, the
// quote that ends a tag blanked, and, when the source is in the old dialect,
// the reserved words in upper case. Strings, character constants and
// comments are left as they are.
//
// Block comments nest for the compiler of today, and so for the grammar.
// They did not always: "/*/ ... //*/" is then one comment, which is switched
// on by taking a slash away. With nested false they are read that way, and
// a comment that the grammar would end elsewhere is taken out of its sight.
func bcplRead(src []byte) bcplText {
	return bcplScan(src, true)
}

func bcplScan(src []byte, nested bool) bcplText {
	text := append([]byte(nil), src...)
	var words []span // Reserved words that are not written in upper case
	type open struct {
		tag   string
		start int
	}
	var opened []open
	var arms []span
	dollarBrackets, braces, tagSet, nesting := false, false, false, false

	n := len(src)
	for i := 0; i < n; {
		c := src[i]
		switch {
		case c == '/' && i+1 < n && src[i+1] == '/', c == '|' && i+1 < n && src[i+1] == '|':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			start, depth, inner := i, 1, false
			for i += 2; i < n && depth > 0; i++ {
				if src[i] == '*' && i+1 < n && src[i+1] == '/' {
					depth--
					i++
				} else if src[i] == '/' && i+1 < n && src[i+1] == '*' {
					inner = true
					if nested {
						depth++
						i++
					}
				}
			}
			if inner {
				nesting = true
				if !nested {
					blank(text, start, min(i, n))
				}
			}
		case c == '"':
			for i++; i < n && src[i] != '"'; i++ {
				if src[i] == '*' {
					i++
				}
			}
			i++
		case c == '\'':
			// 'x' or '*n'; a quote that is neither is left for the parser
			switch {
			case i+3 < n && src[i+1] == '*' && src[i+3] == '\'':
				i += 4
			case i+2 < n && src[i+1] != '*' && src[i+2] == '\'':
				i += 3
			default:
				i++
			}
		case c == '{' || c == '}':
			braces = true
			i++
		case c == '$' && i+1 < n && (src[i+1] == '(' || src[i+1] == ')'):
			dollarBrackets = true
			for i += 2; i < n && isBCPLName(src[i]); i++ {
			}
		case c == '$' && i+1 < n && strings.IndexByte("$<>~", src[i+1]) >= 0:
			kind, start := src[i+1], i
			for i += 2; i < n && isBCPLName(src[i]); i++ {
			}
			tag := strings.ToUpper(string(src[start+2 : i]))
			if i < n && src[i] == '\'' {
				// $<TAG' is the text for when the tag is not set
				text[i] = ' '
				tag += "'"
				i++
			}
			switch kind {
			case '$':
				// $$TAG := value: the rest of the line is the value
				j := i
				for j < n && (src[j] == ' ' || src[j] == '\t') {
					j++
				}
				if j+1 < n && src[j] == ':' && src[j+1] == '=' {
					tagSet = true
					for j < n && src[j] != '\n' && !(src[j] == '/' && j+1 < n && src[j+1] == '/') {
						j++
					}
					blank(text, start, j)
					i = j
				}
			case '<', '~':
				opened = append(opened, open{tag: tag, start: start})
			case '>':
				for k := len(opened) - 1; k >= 0; k-- {
					if opened[k].tag == tag {
						arms = append(arms, span{opened[k].start, i})
						opened = opened[:k]
						break
					}
				}
			}
		case isBCPLNameStart(c):
			start := i
			for i < n && isBCPLName(src[i]) {
				i++
			}
			word := string(src[start:i])
			if upper := strings.ToUpper(word); upper != word && bcplReserved[upper] {
				words = append(words, span{start, i})
			}
		case c >= '0' && c <= '9' || c == '#':
			// A number, so that the x of #x1F and the e of 1e6 are no words
			for i++; i < n && isBCPLName(src[i]); i++ {
			}
		default:
			i++
		}
	}

	old := (dollarBrackets || tagSet) && !braces
	if old {
		for _, w := range words {
			copy(text[w.start:w.end], bytes.ToUpper(text[w.start:w.end]))
		}
	}
	// In the order they open, which is the order they are tried in
	for i := 1; i < len(arms); i++ {
		for j := i; j > 0 && arms[j].start < arms[j-1].start; j-- {
			arms[j], arms[j-1] = arms[j-1], arms[j]
		}
	}
	return bcplText{text: text, old: old, arms: arms, nesting: nesting}
}

// maxBCPLArms is the most conditionally compiled texts that are tried
// without; a file with more is parsed as it is
const maxBCPLArms = 256

// bcplParse parses BCPL in either dialect. Conditional compilation often
// gives two texts for one place, of which the compiler sees one:
//
//	n := $<TRIPOS 5 $>TRIPOS $<RSX 6 $>RSX
//
// Nothing in the file says which tags are set, so both are parsed, and that
// fails. Where it does, each conditional text is left out in turn, and stays
// out when the file then parses with fewer errors.
func bcplParse(parser *sitter.Parser, src []byte) (*sitter.Tree, bool) {
	read := bcplRead(src)
	tree := parser.Parse(read.text, nil)
	if tree == nil {
		return nil, false
	}
	errors := syntaxErrors(tree.RootNode())

	// Comments that only make sense when they do not nest
	if errors > 0 && read.old && read.nesting {
		flat := bcplScan(src, false)
		if without := parser.Parse(flat.text, nil); without != nil {
			if n := syntaxErrors(without.RootNode()); n < errors {
				tree.Close()
				tree, read, errors = without, flat, n
			} else {
				without.Close()
			}
		}
	}
	if errors == 0 || len(read.arms) > maxBCPLArms {
		return tree, read.old
	}

	text := read.text
	for _, arm := range read.arms {
		trial := append([]byte(nil), text...)
		blank(trial, arm.start, arm.end)
		if bytes.Equal(trial, text) {
			continue // Inside one that is left out already
		}
		without := parser.Parse(trial, nil)
		if without == nil {
			continue
		}
		if n := syntaxErrors(without.RootNode()); n < errors {
			tree.Close()
			tree, text, errors = without, trial, n
			if errors == 0 {
				break
			}
		} else {
			without.Close()
		}
	}
	return tree, read.old
}

// syntaxErrors counts the places where a tree does not follow the grammar
func syntaxErrors(n *sitter.Node) int {
	if !n.HasError() {
		return 0
	}
	count := 0
	if n.IsError() || n.IsMissing() {
		count++
	}
	for i := uint(0); i < n.ChildCount(); i++ {
		count += syntaxErrors(n.Child(i))
	}
	return count
}

// What BCPL looks like, to tell it in a file whose name does not: the old
// sources have no extension, and the headers of the new ones are named .h.
var (
	bcplDeclares = []*regexp.Regexp{
		regexp.MustCompile(`(?mi)^[ \t]*(SECTION|NEEDS)[ \t]+"[^"\n]*"`),
		regexp.MustCompile(`(?i)\bMANIFEST\s*(\$\(|\{)`),
		regexp.MustCompile(`(?i)\bGLOBAL\s*(\$\(|\{)`),
		regexp.MustCompile(`(?i)\bSTATIC\s*(\$\(|\{)`),
		regexp.MustCompile(`(?mi)^[ \t]*(LET|AND)[ \t]+[A-Za-z][A-Za-z0-9._]*[ \t]*\([^()\n]*\)[ \t]*(BE\b|=)`),
	}
	bcplUses = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bGET[ \t]+"[^"\n]+"`),
		regexp.MustCompile(`(?s)\$\(.*\$\)`),
		regexp.MustCompile(`(?i)\b(VALOF|RESULTIS|SWITCHON|ENDCASE|REPEATWHILE|REPEATUNTIL)\b`),
		regexp.MustCompile(`\$[$<>][A-Za-z0-9._]+`),
	}
	cDirective = regexp.MustCompile(`(?m)^[ \t]*#[ \t]*(include|define|undef|if|ifdef|ifndef|else|endif|pragma|error)\b`)
)

// sniffSize is how much of a file is read to tell its language
const sniffSize = 32 << 10

// looksLikeBCPL reports whether text is BCPL: it declares something the way
// BCPL does, and shows a second thing that is BCPL's own. One alone is found
// in other files: $( and $) in a Makefile, GET "..." in a description of
// HTTP. What has a line for the C preprocessor is C.
func looksLikeBCPL(src []byte) bool {
	if len(src) > sniffSize {
		src = src[:sniffSize]
	}
	if bytes.IndexByte(src, 0) >= 0 || bytes.HasPrefix(src, []byte("#!")) || cDirective.Match(src) {
		return false
	}
	declares, signs := 0, 0
	for _, re := range bcplDeclares {
		if re.Match(src) {
			declares++
		}
	}
	for _, re := range bcplUses {
		if re.Match(src) {
			signs++
		}
	}
	return declares > 0 && declares+signs >= 2
}
