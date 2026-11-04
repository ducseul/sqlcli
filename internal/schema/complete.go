package schema

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"sqlcli/internal/query"
)

// Kind is what a suggestion refers to.
type Kind int

const (
	KindColumn Kind = iota
	KindTable
	KindKeyword
)

func (k Kind) String() string {
	switch k {
	case KindColumn:
		return "column"
	case KindTable:
		return "table"
	}
	return "keyword"
}

// Item is one autocomplete suggestion.
type Item struct {
	Text   string // inserted in place of the word being typed
	Label  string // shown in the dropdown
	Kind   Kind
	Detail string // e.g. column type and table
}

// MaxItems caps the number of suggestions returned.
const MaxItems = 50

// Keywords offered by the completer (and never taken as table aliases).
var Keywords = []string{
	"SELECT", "FROM", "WHERE", "AND", "OR", "NOT", "NULL", "IS", "IN", "EXISTS", "BETWEEN", "LIKE", "ILIKE",
	"AS", "ON", "USING", "JOIN", "INNER", "LEFT", "RIGHT", "FULL", "OUTER", "CROSS", "NATURAL",
	"GROUP", "BY", "ORDER", "HAVING", "LIMIT", "OFFSET", "FETCH", "FIRST", "NEXT", "ROWS", "ONLY",
	"ASC", "DESC", "DISTINCT", "ALL", "UNION", "INTERSECT", "EXCEPT", "MINUS",
	"INSERT", "INTO", "VALUES", "UPDATE", "SET", "DELETE", "MERGE", "RETURNING",
	"CREATE", "ALTER", "DROP", "TRUNCATE", "TABLE", "VIEW", "INDEX", "SEQUENCE", "SCHEMA", "DATABASE",
	"PRIMARY", "FOREIGN", "KEY", "REFERENCES", "UNIQUE", "CHECK", "DEFAULT", "CONSTRAINT",
	"CASE", "WHEN", "THEN", "ELSE", "END", "WITH", "RECURSIVE", "OVER", "PARTITION",
	"COUNT", "SUM", "AVG", "MIN", "MAX", "COALESCE", "NULLIF", "CAST",
	"BEGIN", "COMMIT", "ROLLBACK", "EXPLAIN", "ANALYZE", "SHOW", "DESCRIBE",
	"TRUE", "FALSE",
}

var keywordSet = func() map[string]bool {
	m := make(map[string]bool, len(Keywords))
	for _, k := range Keywords {
		m[k] = true
	}
	return m
}()

// tableContext keywords: the next word is a table name.
var tableContext = map[string]bool{
	"FROM": true, "JOIN": true, "INTO": true, "UPDATE": true, "TABLE": true, "DESCRIBE": true,
}

func isIdentRune(r rune) bool {
	return r == '_' || r == '$' || r == '#' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// wordBefore returns the start offset of the identifier ending at pos.
func wordBefore(text string, pos int) int {
	start := pos
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:start])
		if !isIdentRune(r) {
			break
		}
		start -= size
	}
	return start
}

// insideLiteral reports whether pos is inside a string literal, quoted
// identifier or comment.
func insideLiteral(text string, pos int) bool {
	inSingle, inDouble, inLine, inBlock := false, false, false, false
	for i := 0; i < pos; i++ {
		c := text[i]
		next := byte(0)
		if i+1 < len(text) {
			next = text[i+1]
		}
		switch {
		case inLine:
			inLine = c != '\n'
		case inBlock:
			if c == '*' && next == '/' {
				inBlock = false
				i++
			}
		case inSingle:
			inSingle = c != '\''
		case inDouble:
			inDouble = c != '"'
		case c == '-' && next == '-':
			inLine = true
			i++
		case c == '/' && next == '*':
			inBlock = true
			i++
		case c == '\'':
			inSingle = true
		case c == '"':
			inDouble = true
		}
	}
	return inSingle || inDouble || inLine || inBlock
}

// statementAround returns the raw segment of text containing pos and pos
// relative to it.
func statementAround(text string, pos int) (string, int) {
	for _, st := range query.SplitRanges(text) {
		if pos >= st.Start && pos <= st.End {
			return text[st.Start:st.End], pos - st.Start
		}
	}
	return text, pos
}

// token is a word or punctuation from tokenize.
type token struct {
	text  string
	start int
}

// tokenize splits SQL into identifiers (dots and quotes kept) and single
// punctuation characters, skipping literals and comments.
func tokenize(s string) []token {
	var out []token
	for i := 0; i < len(s); {
		c := s[i]
		next := byte(0)
		if i+1 < len(s) {
			next = s[i+1]
		}
		switch {
		case c == '-' && next == '-':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '/' && next == '*':
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return out
			}
			i += end + 4
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return out
			}
			i += end + 2
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if isIdentRune(r) || r == '"' || r == '`' {
				start := i
				for i < len(s) {
					r, size := utf8.DecodeRuneInString(s[i:])
					if r == '"' || r == '`' {
						q := byte(r)
						end := strings.IndexByte(s[i+1:], q)
						if end < 0 {
							i = len(s)
							break
						}
						i += end + 2
						continue
					}
					if !isIdentRune(r) && r != '.' {
						break
					}
					i += size
				}
				out = append(out, token{s[start:i], start})
			} else {
				out = append(out, token{s[i : i+size], i})
				i += size
			}
		}
	}
	return out
}

// tableRefs finds tables referenced in stmt and their aliases.
// refs is in order of appearance; aliases maps lower-cased alias -> table.
func tableRefs(stmt string, s *Schema) (refs []*Table, aliases map[string]*Table) {
	aliases = map[string]*Table{}
	seen := map[*Table]bool{}
	toks := tokenize(stmt)
	for i := 0; i < len(toks); i++ {
		kw := strings.ToUpper(toks[i].text)
		if kw != "FROM" && kw != "JOIN" && kw != "UPDATE" && kw != "INTO" {
			continue
		}
		for j := i + 1; j < len(toks); {
			t := s.Lookup(toks[j].text)
			if t == nil {
				break
			}
			if !seen[t] {
				seen[t] = true
				refs = append(refs, t)
			}
			j++
			if j < len(toks) && strings.EqualFold(toks[j].text, "AS") {
				j++
			}
			if j < len(toks) && !keywordSet[strings.ToUpper(toks[j].text)] && toks[j].text != "," && toks[j].text != "(" {
				aliases[strings.ToLower(unquote(toks[j].text))] = t
				j++
			}
			// "FROM a x, b y" lists more tables after a comma.
			if kw == "FROM" && j < len(toks) && toks[j].text == "," {
				j++
				continue
			}
			i = j - 1
			break
		}
	}
	return refs, aliases
}

// insertName is how a table is inserted: PostgreSQL's default "public."
// prefix is dropped.
func insertName(name string) string {
	if rest, ok := strings.CutPrefix(name, "public."); ok {
		return rest
	}
	return name
}

type cand struct {
	Item
	rank int
}

// Complete returns suggestions for the identifier being typed at byte offset
// pos of text, and the offset where that identifier starts (the range
// [start, pos) is what an accepted item replaces). With force, suggestions
// are offered even when nothing has been typed yet (Ctrl+Space).
func Complete(text string, pos int, s *Schema, force bool) (start int, items []Item) {
	if pos < 0 || pos > len(text) || insideLiteral(text, pos) {
		return pos, nil
	}
	start = wordBefore(text, pos)
	word := text[start:pos]
	lw := strings.ToLower(word)

	// Qualified: "alias.", "table." or "schema." before the word.
	if start > 0 && text[start-1] == '.' {
		qend := start - 1
		qstart := wordBefore(text, qend)
		qual := strings.ToLower(text[qstart:qend])
		if qual == "" || s == nil {
			return start, nil
		}
		stmt, _ := statementAround(text, pos)
		_, aliases := tableRefs(stmt, s)
		t := aliases[qual]
		if t == nil {
			// "schema.table." form.
			full := qual
			if qstart > 0 && text[qstart-1] == '.' {
				full = strings.ToLower(text[wordBefore(text, qstart-1):qend])
			}
			t = s.Lookup(full)
		}
		var cs []cand
		if t != nil {
			for _, c := range t.Columns {
				if r := matchRank(c.Name, lw); r >= 0 {
					cs = append(cs, cand{Item{c.Name, c.Name, KindColumn, c.Type}, r})
				}
			}
		} else {
			// Tables of a schema: "public." -> users, orders ...
			for _, t := range s.Tables {
				sch, name, ok := strings.Cut(t.Name, ".")
				if !ok || !strings.EqualFold(sch, qual) {
					continue
				}
				if r := matchRank(name, lw); r >= 0 {
					cs = append(cs, cand{Item{name, name, KindTable, ""}, r})
				}
			}
		}
		return start, finish(cs, word)
	}

	if word == "" && !force {
		return start, nil
	}
	if word != "" && unicode.IsDigit([]rune(word)[0]) {
		return start, nil
	}

	stmt, rel := statementAround(text, pos)
	refs, _ := tableRefs(stmt, s)

	// Previous token decides whether a table name is expected.
	wantTable := false
	toks := tokenize(stmt[:rel-len(word)])
	if n := len(toks); n > 0 {
		prev := strings.ToUpper(toks[n-1].text)
		wantTable = tableContext[prev] || (prev == "," && fromListContext(toks))
	}

	var cs []cand
	add := func(it Item, base, r int) {
		if r >= 0 {
			cs = append(cs, cand{it, base + r})
		}
	}
	if s != nil {
		tableBase, colBase := 0, 100
		if !wantTable {
			tableBase, colBase = 200, 0
		}
		for _, t := range s.Tables {
			name := insertName(t.Name)
			r := matchRank(name, lw)
			if alt := matchRank(t.Name, lw); r < 0 || (alt >= 0 && alt < r) {
				r = alt
			}
			add(Item{name, t.Name, KindTable, ""}, tableBase, r)
		}
		if !wantTable {
			for _, t := range refs {
				for _, c := range t.Columns {
					add(Item{c.Name, c.Name, KindColumn, c.Type + " · " + t.Name}, colBase, matchRank(c.Name, lw))
				}
			}
			if len(refs) == 0 && word != "" {
				// No FROM yet: any column of any table, lowest priority.
				seen := map[string]bool{}
				for _, t := range s.Tables {
					for _, c := range t.Columns {
						k := strings.ToLower(c.Name)
						if seen[k] {
							continue
						}
						seen[k] = true
						add(Item{c.Name, c.Name, KindColumn, c.Type + " · " + t.Name}, 400, matchRank(c.Name, lw))
					}
				}
			}
		}
	}
	if !wantTable {
		lower := word != "" && word == strings.ToLower(word)
		for _, k := range Keywords {
			ins := k
			if lower {
				ins = strings.ToLower(k)
			}
			add(Item{ins, k, KindKeyword, ""}, 300, matchRank(k, lw))
		}
	}
	return start, finish(cs, word)
}

// fromListContext reports whether the last clause keyword before a trailing
// comma is FROM (so "FROM a, |" expects a table).
func fromListContext(toks []token) bool {
	depth := 0
	for i := len(toks) - 1; i >= 0; i-- {
		switch t := strings.ToUpper(toks[i].text); t {
		case ")":
			depth++
		case "(":
			depth--
		case "FROM":
			if depth == 0 {
				return true
			}
		case "SELECT", "WHERE", "SET", "ON", "GROUP", "ORDER", "VALUES", "BY":
			if depth == 0 {
				return false
			}
		}
	}
	return false
}

// matchRank returns 0 for a prefix match, 50 for a substring match (only
// with 2+ typed chars) and -1 for no match. An empty word matches all.
func matchRank(name, lw string) int {
	ln := strings.ToLower(name)
	switch {
	case strings.HasPrefix(ln, lw):
		return 0
	case len(lw) >= 2 && strings.Contains(ln, lw):
		return 50
	}
	return -1
}

// finish sorts, de-duplicates and caps the candidates. A lone suggestion that
// equals what is already typed is dropped.
func finish(cs []cand, word string) []Item {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].rank != cs[j].rank {
			return cs[i].rank < cs[j].rank
		}
		return strings.ToLower(cs[i].Label) < strings.ToLower(cs[j].Label)
	})
	seen := map[string]bool{}
	var out []Item
	for _, c := range cs {
		k := strings.ToLower(c.Text) + "\x00" + c.Kind.String()
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, c.Item)
		if len(out) >= MaxItems {
			break
		}
	}
	if len(out) == 1 && strings.EqualFold(out[0].Text, word) {
		return nil
	}
	return out
}
