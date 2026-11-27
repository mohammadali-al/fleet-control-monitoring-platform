// Package baseline stores the global library of reusable command templates
// ("baseline items") and provides the placeholder validation and substitution
// engine shared with Maneuvers and the operator panel.
package baseline

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors returned by template parsing, substitution, and store ops.
var (
	ErrNotFound              = errors.New("baseline: item not found")
	ErrAlreadyExists         = errors.New("baseline: item already exists")
	ErrInvalidItem           = errors.New("baseline: invalid item")
	ErrInvalidTemplate       = errors.New("baseline: invalid command template")
	ErrUnresolvedPlaceholder = errors.New("baseline: unresolved placeholder")
)

// UnresolvedPlaceholdersError describes one or more placeholders that had no
// non-empty value during template rendering.
type UnresolvedPlaceholdersError struct {
	Missing []string
}

func (e *UnresolvedPlaceholdersError) Error() string {
	return fmt.Sprintf("%s: %s", ErrUnresolvedPlaceholder, formatPlaceholderList(e.Missing))
}

func (e *UnresolvedPlaceholdersError) Unwrap() error {
	return ErrUnresolvedPlaceholder
}

func formatPlaceholderList(names []string) string {
	wrapped := make([]string, len(names))
	for i, n := range names {
		wrapped[i] = "{" + n + "}"
	}
	return strings.Join(wrapped, ", ")
}

// Token represents a parsed segment of a command template: either literal text
// or a named placeholder reference.
type Token struct {
	Literal string
	Name    string // non-empty when this token is a placeholder
}

// IsPlaceholderName reports whether name is a valid placeholder identifier:
// starts with an ASCII letter or underscore, followed by ASCII letters, digits,
// underscores, or hyphens (e.g. rate, duration, ip, domain, url, target_port).
func IsPlaceholderName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
		isDigit := c >= '0' && c <= '9'
		if i == 0 {
			if !isAlpha {
				return false
			}
			continue
		}
		if !isAlpha && !isDigit && c != '-' {
			return false
		}
	}
	return true
}

// ParseTemplate parses tmpl into literal and placeholder tokens and returns the
// deduplicated list of placeholder names in order of first appearance.
//
// Placeholder syntax:
//   - Placeholders are written as {name}, where name satisfies IsPlaceholderName.
//   - Literal braces are escaped as {{ and }}.
//   - Empty templates, NUL bytes, unclosed {, unmatched }, empty {}, nested
//     braces inside a placeholder, or invalid placeholder names fail with
//     ErrInvalidTemplate.
func ParseTemplate(tmpl string) ([]Token, []string, error) {
	if strings.TrimSpace(tmpl) == "" {
		return nil, nil, fmt.Errorf("%w: template is empty", ErrInvalidTemplate)
	}
	if strings.IndexByte(tmpl, 0) >= 0 {
		return nil, nil, fmt.Errorf("%w: template contains NUL byte", ErrInvalidTemplate)
	}

	var tokens []Token
	var placeholders []string
	seen := map[string]bool{}
	var lit strings.Builder
	flushLit := func() {
		if lit.Len() > 0 {
			tokens = append(tokens, Token{Literal: lit.String()})
			lit.Reset()
		}
	}

	for i := 0; i < len(tmpl); {
		ch := tmpl[i]
		switch ch {
		case '{':
			if i+1 < len(tmpl) && tmpl[i+1] == '{' {
				lit.WriteByte('{')
				i += 2
				continue
			}
			end := strings.IndexByte(tmpl[i+1:], '}')
			if end < 0 {
				return nil, nil, fmt.Errorf("%w: unclosed '{' at offset %d", ErrInvalidTemplate, i)
			}
			name := tmpl[i+1 : i+1+end]
			if name == "" {
				return nil, nil, fmt.Errorf("%w: empty placeholder '{}' at offset %d", ErrInvalidTemplate, i)
			}
			if strings.IndexByte(name, '{') >= 0 {
				return nil, nil, fmt.Errorf("%w: nested '{' inside placeholder at offset %d", ErrInvalidTemplate, i)
			}
			if !IsPlaceholderName(name) {
				return nil, nil, fmt.Errorf("%w: invalid placeholder name %q (must start with letter/underscore and contain only letters, digits, '_' or '-')", ErrInvalidTemplate, name)
			}
			flushLit()
			tokens = append(tokens, Token{Name: name})
			if !seen[name] {
				seen[name] = true
				placeholders = append(placeholders, name)
			}
			i += 1 + end + 1
		case '}':
			if i+1 < len(tmpl) && tmpl[i+1] == '}' {
				lit.WriteByte('}')
				i += 2
				continue
			}
			return nil, nil, fmt.Errorf("%w: unmatched '}' at offset %d", ErrInvalidTemplate, i)
		default:
			lit.WriteByte(ch)
			i++
		}
	}
	flushLit()
	return tokens, placeholders, nil
}

// ValidateTemplate validates tmpl and returns its unique placeholder names in
// order of first appearance.
func ValidateTemplate(tmpl string) ([]string, error) {
	_, placeholders, err := ParseTemplate(tmpl)
	return placeholders, err
}

// RenderResult is the materialized output of substituting inputs into a template.
type RenderResult struct {
	Command      string            `json:"command"`
	Placeholders []string          `json:"placeholders"`
	Resolved     map[string]string `json:"resolved"`
}

// Render validates tmpl and substitutes placeholders using defaults overlaid
// with inputs. Every placeholder referenced in tmpl must have a non-empty
// value in inputs or defaults; otherwise Render fails with
// *UnresolvedPlaceholdersError (wrapping ErrUnresolvedPlaceholder).
//
// Substitution is single-pass: placeholder-like text inside substituted values
// is treated as literal output and never re-expanded.
func Render(tmpl string, defaults, inputs map[string]string) (RenderResult, error) {
	tokens, placeholders, err := ParseTemplate(tmpl)
	if err != nil {
		return RenderResult{}, err
	}

	resolved := make(map[string]string, len(placeholders))
	var missing []string
	for _, name := range placeholders {
		val := ""
		if v, ok := inputs[name]; ok && v != "" {
			val = v
		} else if v, ok := defaults[name]; ok && v != "" {
			val = v
		}
		if val == "" {
			missing = append(missing, name)
			continue
		}
		if strings.IndexByte(val, 0) >= 0 {
			return RenderResult{}, fmt.Errorf("%w: input for {%s} contains NUL byte", ErrInvalidItem, name)
		}
		resolved[name] = val
	}
	if len(missing) > 0 {
		return RenderResult{}, &UnresolvedPlaceholdersError{Missing: missing}
	}

	var out strings.Builder
	for _, tok := range tokens {
		if tok.Name != "" {
			out.WriteString(resolved[tok.Name])
		} else {
			out.WriteString(tok.Literal)
		}
	}
	if placeholders == nil {
		placeholders = []string{}
	}
	return RenderResult{
		Command:      out.String(),
		Placeholders: placeholders,
		Resolved:     resolved,
	}, nil
}

// RenderItem materializes item.CommandTemplate using item.Defaults overlaid
// with inputs.
func RenderItem(item Item, inputs map[string]string) (RenderResult, error) {
	return Render(item.CommandTemplate, item.Defaults, inputs)
}
