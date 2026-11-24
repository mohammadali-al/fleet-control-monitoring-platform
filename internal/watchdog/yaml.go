package watchdog

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// This file is a small parser for the YAML subset the policy profile uses. The
// module carries no YAML library and adding one is outside this task's touches,
// so the subset is parsed here and any construct outside it is rejected loudly
// rather than misread:
//
//   - block mappings (spaces only, any consistent indent)
//   - block sequences of scalars ("- item")
//   - flow sequences and flow mappings ("[a, b]", "{trigger: 97, recover: 92}")
//   - scalars: plain, "double" or 'single' quoted, integers, floats, true/false, null/~
//   - "#" comments and blank lines, and an optional leading "---"
//
// Not supported (an error, never silently ignored): tabs for indentation,
// anchors/aliases/tags, multi-line scalars, multiple documents, duplicate keys,
// lists of mappings.

// parseYAML parses a document made of one top-level mapping into
// map[string]any with string, int64, float64, bool, nil, []any and
// map[string]any values.
func parseYAML(src []byte) (map[string]any, error) {
	lines, err := yamlLines(string(src))
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, errors.New("yaml: empty document")
	}
	p := &yamlParser{lines: lines}
	if lines[0].indent != 0 {
		return nil, fmt.Errorf("yaml: line %d: top level must not be indented", lines[0].no)
	}
	m, err := p.mapping(0)
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.lines) {
		return nil, fmt.Errorf("yaml: line %d: unexpected content", p.lines[p.pos].no)
	}
	return m, nil
}

type yamlLine struct {
	no     int
	indent int
	text   string // trimmed, comment stripped
}

func yamlLines(src string) ([]yamlLine, error) {
	var out []yamlLine
	for i, raw := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		no := i + 1
		trimmedLeft := strings.TrimLeft(raw, " ")
		if strings.HasPrefix(trimmedLeft, "\t") {
			return nil, fmt.Errorf("yaml: line %d: tabs are not allowed for indentation", no)
		}
		text := strings.TrimSpace(stripComment(trimmedLeft))
		if text == "" {
			continue
		}
		if text == "---" && len(out) == 0 {
			continue
		}
		if text == "---" || text == "..." {
			return nil, fmt.Errorf("yaml: line %d: multiple documents are not supported", no)
		}
		out = append(out, yamlLine{no: no, indent: len(raw) - len(trimmedLeft), text: text})
	}
	return out, nil
}

// stripComment removes a trailing "# ..." comment that is not inside quotes.
func stripComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			// A quote only opens a string at the start of a token.
			if i == 0 || strings.ContainsRune(" :[{,-", rune(s[i-1])) {
				quote = c
			}
		case c == '#' && (i == 0 || s[i-1] == ' '):
			return s[:i]
		}
	}
	return s
}

type yamlParser struct {
	lines []yamlLine
	pos   int
}

func isListItem(text string) bool {
	return strings.HasPrefix(text, "- ") || text == "-"
}

func (p *yamlParser) mapping(indent int) (map[string]any, error) {
	m := map[string]any{}
	for p.pos < len(p.lines) {
		ln := p.lines[p.pos]
		if ln.indent < indent {
			break
		}
		if ln.indent > indent {
			return nil, fmt.Errorf("yaml: line %d: unexpected indentation", ln.no)
		}
		if isListItem(ln.text) {
			return nil, fmt.Errorf("yaml: line %d: unexpected list item in a mapping", ln.no)
		}
		key, rest, err := splitKey(ln)
		if err != nil {
			return nil, err
		}
		if _, dup := m[key]; dup {
			return nil, fmt.Errorf("yaml: line %d: duplicate key %q", ln.no, key)
		}
		p.pos++
		if rest != "" {
			v, err := parseValue(rest, ln.no)
			if err != nil {
				return nil, err
			}
			m[key] = v
			continue
		}
		// Empty value: a nested block, a list, or null when nothing is nested.
		if p.pos >= len(p.lines) {
			m[key] = nil
			continue
		}
		next := p.lines[p.pos]
		switch {
		case next.indent > indent && isListItem(next.text):
			l, err := p.list(next.indent)
			if err != nil {
				return nil, err
			}
			m[key] = l
		case next.indent > indent:
			sub, err := p.mapping(next.indent)
			if err != nil {
				return nil, err
			}
			m[key] = sub
		case next.indent == indent && isListItem(next.text):
			// A list may sit at the same indent as its key ("key:\n- a").
			l, err := p.list(indent)
			if err != nil {
				return nil, err
			}
			m[key] = l
		default:
			m[key] = nil
		}
	}
	return m, nil
}

func (p *yamlParser) list(indent int) ([]any, error) {
	out := []any{}
	for p.pos < len(p.lines) {
		ln := p.lines[p.pos]
		if ln.indent != indent || !isListItem(ln.text) {
			break
		}
		item := strings.TrimSpace(strings.TrimPrefix(ln.text, "-"))
		if item == "" {
			return nil, fmt.Errorf("yaml: line %d: empty list item", ln.no)
		}
		if !strings.HasPrefix(item, "[") && !strings.HasPrefix(item, "{") && !isQuoted(item) {
			if _, _, err := splitKey(yamlLine{no: ln.no, text: item}); err == nil {
				return nil, fmt.Errorf("yaml: line %d: lists of mappings are not supported (use a mapping keyed by name)", ln.no)
			}
		}
		v, err := parseValue(item, ln.no)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.pos++
	}
	return out, nil
}

func isQuoted(s string) bool {
	return len(s) > 0 && (s[0] == '"' || s[0] == '\'')
}

// splitKey splits "key: rest" at the first colon that ends a key.
func splitKey(ln yamlLine) (key, rest string, err error) {
	t := ln.text
	if isQuoted(t) {
		end := closingQuote(t)
		if end < 0 || end+1 >= len(t) || t[end+1] != ':' {
			return "", "", fmt.Errorf("yaml: line %d: expected \"key: value\"", ln.no)
		}
		k, err := unquote(t[:end+1], ln.no)
		if err != nil {
			return "", "", err
		}
		return k, strings.TrimSpace(t[end+2:]), nil
	}
	for i := 0; i < len(t); i++ {
		if t[i] == ':' && (i+1 == len(t) || t[i+1] == ' ') {
			k := strings.TrimSpace(t[:i])
			if k == "" {
				break
			}
			if strings.ContainsAny(k, "[]{}&*!|>%@`") {
				return "", "", fmt.Errorf("yaml: line %d: unsupported key %q", ln.no, k)
			}
			return k, strings.TrimSpace(t[i+1:]), nil
		}
	}
	return "", "", fmt.Errorf("yaml: line %d: expected \"key: value\"", ln.no)
}

func closingQuote(s string) int {
	q := s[0]
	for i := 1; i < len(s); i++ {
		if s[i] == '\\' && q == '"' {
			i++
			continue
		}
		if s[i] == q {
			if q == '\'' && i+1 < len(s) && s[i+1] == '\'' {
				i++ // '' is an escaped single quote
				continue
			}
			return i
		}
	}
	return -1
}

func unquote(s string, no int) (string, error) {
	if len(s) < 2 || closingQuote(s) != len(s)-1 {
		return "", fmt.Errorf("yaml: line %d: bad quoted string %s", no, s)
	}
	if s[0] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	}
	u, err := strconv.Unquote(s)
	if err != nil {
		return "", fmt.Errorf("yaml: line %d: bad quoted string %s", no, s)
	}
	return u, nil
}

// parseValue parses an inline value: a flow collection or a scalar.
func parseValue(s string, no int) (any, error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "[") || strings.HasPrefix(s, "{"):
		fp := &flowParser{s: s, no: no}
		v, err := fp.value()
		if err != nil {
			return nil, err
		}
		fp.skip()
		if fp.i != len(fp.s) {
			return nil, fmt.Errorf("yaml: line %d: trailing text after flow value", no)
		}
		return v, nil
	case strings.HasPrefix(s, "|") || strings.HasPrefix(s, ">"):
		return nil, fmt.Errorf("yaml: line %d: multi-line scalars are not supported", no)
	case strings.HasPrefix(s, "&") || strings.HasPrefix(s, "*") || strings.HasPrefix(s, "!"):
		return nil, fmt.Errorf("yaml: line %d: anchors, aliases and tags are not supported", no)
	}
	return scalar(s, no)
}

func scalar(s string, no int) (any, error) {
	if isQuoted(s) {
		return unquote(s, no)
	}
	switch s {
	case "true", "True":
		return true, nil
	case "false", "False":
		return false, nil
	case "null", "~", "Null":
		return nil, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	// Only plain decimal floats; "nan"/"inf"/"0x.." stay strings.
	if strings.Trim(s, "0123456789.-+eE") == "" {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, nil
		}
	}
	return s, nil
}

type flowParser struct {
	s  string
	i  int
	no int
}

func (f *flowParser) skip() {
	for f.i < len(f.s) && f.s[f.i] == ' ' {
		f.i++
	}
}

func (f *flowParser) value() (any, error) {
	f.skip()
	if f.i >= len(f.s) {
		return nil, fmt.Errorf("yaml: line %d: unterminated flow value", f.no)
	}
	switch f.s[f.i] {
	case '[':
		f.i++
		out := []any{}
		for {
			f.skip()
			if f.i >= len(f.s) {
				return nil, fmt.Errorf("yaml: line %d: unterminated flow sequence", f.no)
			}
			if f.s[f.i] == ']' {
				f.i++
				return out, nil
			}
			v, err := f.value()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			f.skip()
			if f.i < len(f.s) && f.s[f.i] == ',' {
				f.i++
			}
		}
	case '{':
		f.i++
		out := map[string]any{}
		for {
			f.skip()
			if f.i >= len(f.s) {
				return nil, fmt.Errorf("yaml: line %d: unterminated flow mapping", f.no)
			}
			if f.s[f.i] == '}' {
				f.i++
				return out, nil
			}
			kv, err := f.token(true)
			if err != nil {
				return nil, err
			}
			k, ok := kv.(string)
			if !ok {
				return nil, fmt.Errorf("yaml: line %d: flow mapping keys must be strings", f.no)
			}
			f.skip()
			if f.i >= len(f.s) || f.s[f.i] != ':' {
				return nil, fmt.Errorf("yaml: line %d: expected ':' in flow mapping", f.no)
			}
			f.i++
			v, err := f.value()
			if err != nil {
				return nil, err
			}
			if _, dup := out[k]; dup {
				return nil, fmt.Errorf("yaml: line %d: duplicate key %q", f.no, k)
			}
			out[k] = v
			f.skip()
			if f.i < len(f.s) && f.s[f.i] == ',' {
				f.i++
			}
		}
	default:
		return f.token(false)
	}
}

// token reads a scalar up to a flow delimiter. Keys are always strings.
func (f *flowParser) token(isKey bool) (any, error) {
	f.skip()
	start := f.i
	if f.i < len(f.s) && (f.s[f.i] == '"' || f.s[f.i] == '\'') {
		end := closingQuote(f.s[f.i:])
		if end < 0 {
			return nil, fmt.Errorf("yaml: line %d: unterminated quoted string", f.no)
		}
		f.i += end + 1
		return unquote(f.s[start:f.i], f.no)
	}
	for f.i < len(f.s) {
		c := f.s[f.i]
		if c == ',' || c == ']' || c == '}' || (isKey && c == ':') {
			break
		}
		f.i++
	}
	tok := strings.TrimSpace(f.s[start:f.i])
	if tok == "" {
		return nil, fmt.Errorf("yaml: line %d: empty value in flow collection", f.no)
	}
	if isKey {
		return tok, nil
	}
	return scalar(tok, f.no)
}
