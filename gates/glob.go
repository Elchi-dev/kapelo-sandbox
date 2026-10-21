package gates

import (
	"regexp"
	"strings"
)

// Glob matches slash separated paths. "**/" matches any number of
// directories (including none), "**" anything, "*" anything but "/",
// "?" one character but "/".
type Glob struct {
	re *regexp.Regexp
}

func CompileGlob(pattern string) (Glob, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch {
		case strings.HasPrefix(pattern[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return Glob{}, err
	}
	return Glob{re: re}, nil
}

func (g Glob) Match(path string) bool { return g.re.MatchString(path) }

// MatchAny reports whether path matches one of the patterns. Invalid
// patterns never match.
func MatchAny(patterns []string, path string) bool {
	for _, p := range patterns {
		g, err := CompileGlob(p)
		if err == nil && g.Match(path) {
			return true
		}
	}
	return false
}
