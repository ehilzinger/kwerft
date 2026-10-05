package compose

import (
	"fmt"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// interpolator resolves ${VAR} references as Compose does, from the values
// the user gave (a .env file's). The console's own environment is never
// consulted: it would leak the server's settings into an App.
type interpolator struct {
	env     map[string]string
	missing map[string]bool
}

func isNameStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isNameChar(c byte) bool { return isNameStart(c) || c >= '0' && c <= '9' }

func (in *interpolator) lookup(name string) (string, bool) {
	v, ok := in.env[name]
	return v, ok
}

// expand resolves every reference in s: $$ is a literal $, $VAR and ${VAR}
// a variable, with the modifiers :- - :? ? :+ + (the word after them may
// hold references too).
func (in *interpolator) expand(s string) (string, error) {
	if !strings.Contains(s, "$") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c != '$' || i+1 >= len(s) {
			b.WriteByte(c)
			i++
			continue
		}
		switch n := s[i+1]; {
		case n == '$':
			b.WriteByte('$')
			i += 2
		case n == '{':
			end := closingBrace(s, i+2)
			if end < 0 {
				return "", fmt.Errorf("%q has a ${ without its closing }", s)
			}
			v, err := in.braced(s[i+2 : end])
			if err != nil {
				return "", err
			}
			b.WriteString(v)
			i = end + 1
		case isNameStart(n):
			j := i + 1
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			name := s[i+1 : j]
			v, ok := in.lookup(name)
			if !ok {
				in.missing[name] = true
			}
			b.WriteString(v)
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

// closingBrace finds the } that closes a ${ whose content starts at from.
func closingBrace(s string, from int) int {
	depth := 1
	for k := from; k < len(s); k++ {
		switch {
		case s[k] == '$' && k+1 < len(s) && s[k+1] == '{':
			depth++
			k++
		case s[k] == '}':
			depth--
			if depth == 0 {
				return k
			}
		}
	}
	return -1
}

func (in *interpolator) braced(expr string) (string, error) {
	j := 0
	for j < len(expr) && isNameChar(expr[j]) {
		j++
	}
	name, rest := expr[:j], expr[j:]
	if name == "" || !isNameStart(name[0]) {
		return "", fmt.Errorf("${%s} is not a variable reference", expr)
	}
	v, set := in.lookup(name)
	for _, op := range []string{":-", ":?", ":+", "-", "?", "+"} {
		if !strings.HasPrefix(rest, op) {
			continue
		}
		word := rest[len(op):]
		unset := !set || strings.HasPrefix(op, ":") && v == ""
		switch op[len(op)-1] {
		case '-':
			if unset {
				return in.expand(word)
			}
			return v, nil
		case '?':
			if unset {
				msg, _ := in.expand(word)
				if msg == "" {
					msg = "it is required"
				}
				return "", fmt.Errorf("the variable %s is not set: %s", name, msg)
			}
			return v, nil
		case '+':
			if unset {
				return "", nil
			}
			return in.expand(word)
		}
	}
	if rest != "" {
		return "", fmt.Errorf("${%s} is not a variable reference Compose knows", expr)
	}
	if !set {
		in.missing[name] = true
	}
	return v, nil
}

// interpolate resolves the references in every value of the document (not
// in keys), before anything reads it, as Compose does. An alias is the
// node its anchor names, resolved where the anchor stands.
func (in *interpolator) interpolate(n *yaml.Node) error {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if err := in.interpolate(c); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if err := in.interpolate(n.Content[i+1]); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		v, err := in.expand(n.Value)
		if err != nil {
			return err
		}
		n.Value = v
	}
	return nil
}

func (in *interpolator) missingNames() []string {
	out := make([]string, 0, len(in.missing))
	for k := range in.missing {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// splitShell splits a command line into words like a POSIX shell, without
// expanding anything: quotes group, backslash escapes. Compose does the same
// with a string command or entrypoint.
func splitShell(s string) ([]string, error) {
	var (
		words []string
		cur   strings.Builder
		in    bool // inside a word
		quote byte
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case quote == '"':
			switch {
			case c == '"':
				quote = 0
			case c == '\\' && i+1 < len(s) && strings.IndexByte("\\\"$`\n", s[i+1]) >= 0:
				i++
				cur.WriteByte(s[i])
			default:
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, in = c, true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			in = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unclosed %c quote", quote)
	}
	if in {
		words = append(words, cur.String())
	}
	return words, nil
}
