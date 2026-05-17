package logquery

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Matcher represents a label selector matcher found in the query.
type Matcher struct {
	Name  string
	Op    string
	Value string
}

// LineFilter represents a LogQL-compatible line filter stage.
type LineFilter struct {
	Op    string
	Value string
}

// Query captures the MonoFS log query subset currently used by the logengine.
//
// It intentionally supports a narrow compatibility surface today:
// common label selector operators and line-filter stages.
type Query struct {
	Matchers    []Matcher
	LineFilters []LineFilter
}

// Parse parses the MonoFS-compatible log query subset.
func Parse(input string) (Query, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return Query{}, fmt.Errorf("query cannot be empty")
	}

	query := Query{}
	rest := trimmed
	if strings.HasPrefix(rest, "{") {
		matchers, remaining, err := parseSelector(rest)
		if err != nil {
			return Query{}, err
		}
		query.Matchers = matchers
		rest = remaining
	}

	for {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return query, nil
		}

		filter, remaining, ok, err := parseLineFilter(rest)
		if err != nil {
			return Query{}, err
		}
		if ok {
			query.LineFilters = append(query.LineFilters, filter)
			rest = remaining
			continue
		}

		if strings.HasPrefix(rest, "|") {
			remaining, err := skipPipelineStage(rest[1:])
			if err != nil {
				return Query{}, err
			}
			rest = remaining
			continue
		}

		return Query{}, fmt.Errorf("unsupported query fragment %q", rest)
	}
}

// ServiceEquals returns the first exact service matcher when present.
func (q Query) ServiceEquals() string {
	for _, matcher := range q.Matchers {
		if matcher.Name == "service" && matcher.Op == "=" {
			return matcher.Value
		}
	}
	return ""
}

// PositiveLineContainsFilters returns all positive substring line filters.
func (q Query) PositiveLineContainsFilters() []string {
	filters := make([]string, 0, len(q.LineFilters))
	for _, filter := range q.LineFilters {
		if filter.Op == "|=" {
			filters = append(filters, filter.Value)
		}
	}
	return filters
}

func parseSelector(input string) ([]Matcher, string, error) {
	end, err := findSelectorEnd(input)
	if err != nil {
		return nil, "", err
	}

	body := strings.TrimSpace(input[1:end])
	rest := input[end+1:]
	if body == "" {
		return nil, rest, nil
	}

	parts, err := splitOutsideQuotes(body, ',')
	if err != nil {
		return nil, "", err
	}
	matchers := make([]Matcher, 0, len(parts))
	for _, part := range parts {
		matcher, err := parseMatcher(strings.TrimSpace(part))
		if err != nil {
			return nil, "", err
		}
		matchers = append(matchers, matcher)
	}
	return matchers, rest, nil
}

func findSelectorEnd(input string) (int, error) {
	inQuote := false
	escaped := false
	for i := 1; i < len(input); i++ {
		switch input[i] {
		case '\\':
			if inQuote && !escaped {
				escaped = true
				continue
			}
		case '"':
			if !escaped {
				inQuote = !inQuote
			}
		case '}':
			if !inQuote {
				return i, nil
			}
		}
		escaped = false
	}
	return 0, fmt.Errorf("unterminated selector")
}

func splitOutsideQuotes(input string, separator byte) ([]string, error) {
	var parts []string
	start := 0
	inQuote := false
	escaped := false
	for i := 0; i < len(input); i++ {
		switch input[i] {
		case '\\':
			if inQuote && !escaped {
				escaped = true
				continue
			}
		case '"':
			if !escaped {
				inQuote = !inQuote
			}
		case separator:
			if !inQuote {
				parts = append(parts, input[start:i])
				start = i + 1
			}
		}
		escaped = false
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quoted string")
	}
	parts = append(parts, input[start:])
	return parts, nil
}

func parseMatcher(input string) (Matcher, error) {
	if input == "" {
		return Matcher{}, fmt.Errorf("empty matcher")
	}

	for _, op := range []string{"=~", "!~", "!=", "="} {
		idx := strings.Index(input, op)
		if idx < 0 {
			continue
		}
		name := strings.TrimSpace(input[:idx])
		if name == "" {
			return Matcher{}, fmt.Errorf("missing matcher name in %q", input)
		}
		value, rest, err := consumeQuotedString(strings.TrimSpace(input[idx+len(op):]))
		if err != nil {
			return Matcher{}, fmt.Errorf("invalid matcher %q: %w", input, err)
		}
		if strings.TrimSpace(rest) != "" {
			return Matcher{}, fmt.Errorf("unexpected trailing content in matcher %q", input)
		}
		return Matcher{Name: name, Op: op, Value: value}, nil
	}

	return Matcher{}, fmt.Errorf("unsupported matcher %q", input)
}

func parseLineFilter(input string) (LineFilter, string, bool, error) {
	for _, op := range []string{"|=", "!=", "|~", "!~"} {
		if !strings.HasPrefix(input, op) {
			continue
		}
		value, rest, err := consumeQuotedString(strings.TrimSpace(input[len(op):]))
		if err != nil {
			return LineFilter{}, "", true, fmt.Errorf("invalid line filter: %w", err)
		}
		return LineFilter{Op: op, Value: value}, rest, true, nil
	}
	return LineFilter{}, input, false, nil
}

func consumeQuotedString(input string) (string, string, error) {
	if input == "" {
		return "", "", fmt.Errorf("expected quoted string")
	}

	quote, width := utf8.DecodeRuneInString(input)
	if quote != '"' && quote != '\'' && quote != '`' {
		return "", "", fmt.Errorf("expected quoted string")
	}

	escaped := false
	for i := width; i < len(input); i++ {
		if input[i] == '\\' && quote != '`' && !escaped {
			escaped = true
			continue
		}
		if rune(input[i]) == quote && !escaped {
			raw := input[:i+1]
			value, err := strconv.Unquote(raw)
			if err != nil {
				return "", "", err
			}
			return value, input[i+1:], nil
		}
		escaped = false
	}

	return "", "", fmt.Errorf("unterminated quoted string")
}

func skipPipelineStage(input string) (string, error) {
	inQuote := rune(0)
	escaped := false
	depth := 0
	for i, r := range input {
		switch {
		case inQuote != 0:
			if r == '\\' && inQuote != '`' && !escaped {
				escaped = true
				continue
			}
			if r == inQuote && !escaped {
				inQuote = 0
			}
			escaped = false
		case r == '"' || r == '\'' || r == '`':
			inQuote = r
		case r == '(' || r == '[' || r == '{':
			depth++
		case r == ')' || r == ']' || r == '}':
			if depth > 0 {
				depth--
			}
		case depth == 0 && startsLineFilterAt(input, i):
			return input[i:], nil
		}
	}
	if inQuote != 0 {
		return "", fmt.Errorf("unterminated quoted string in pipeline stage")
	}
	return "", nil
}

func startsLineFilterAt(input string, index int) bool {
	rest := input[index:]
	if strings.HasPrefix(rest, "|=") || strings.HasPrefix(rest, "|~") {
		return true
	}
	if !strings.HasPrefix(rest, "!=") && !strings.HasPrefix(rest, "!~") {
		return false
	}
	if index == 0 {
		return true
	}
	prev, _ := utf8.DecodeLastRuneInString(input[:index])
	return unicode.IsSpace(prev)
}
