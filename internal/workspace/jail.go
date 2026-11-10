package workspace

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The lexical limits on a path.
const (
	// MaxPathBytes is the longest path accepted. It matches PATH_MAX on
	// macOS, the smaller of the supported systems.
	MaxPathBytes = 1024
	// MaxComponentBytes is the longest single name accepted.
	MaxComponentBytes = 255
	// MaxPathDepth is the most components a path may have.
	MaxPathDepth = 64
)

// Policy is the lexical half of the jail: it validates a path as text and
// says whether it is protected. It never touches the file system. Symlinks
// are handled by Local, which applies the policy again to every path a
// symlink leads to.
type Policy struct {
	rules []prefixRule
}

// prefixRule protects one path anchored at the checkout root, and everything
// below it.
type prefixRule struct {
	keys    []string // the match key of each component
	display string
}

// NewPolicy returns the built-in rules plus the extra protected paths. The
// built-in rules are:
//
//   - a component named .git at any depth
//   - a component whose name starts with .env at any depth (.env, .env.local,
//     .envrc)
//   - .github/workflows at the checkout root
//
// Names are compared after case folding, so a variant such as .GIT or .Env
// that a case-insensitive file system treats as the same file is protected
// too. Combining marks (U+0300–U+036F and Unicode Mn/Me) are rejected
// in both configured rules and candidate paths. Without a normalization
// dependency this conservatively refuses decomposed spellings, including
// symlink targets, rather than allowing NFC/NFD aliases to bypass a rule.
func NewPolicy(extra []string) (*Policy, error) {
	p := &Policy{rules: []prefixRule{{keys: []string{".github", "workflows"}, display: ".github/workflows"}}}
	for _, raw := range extra {
		clean, err := cleanPath(raw)
		if err != nil {
			return nil, fmt.Errorf("protected path %q: %w", raw, err)
		}
		if clean == "." {
			return nil, fmt.Errorf("protected path %q: it would protect the whole checkout", raw)
		}
		rule := prefixRule{display: clean}
		for name := range strings.SplitSeq(clean, "/") {
			key := matchKey(name)
			if key == "" {
				return nil, fmt.Errorf("protected path %q: component %q cannot be matched", raw, name)
			}
			rule.keys = append(rule.keys, key)
		}
		p.rules = append(p.rules, rule)
	}
	return p, nil
}

// lexError is a lexical failure before it is tied to an operation.
type lexError struct {
	kind   error
	detail string
}

func (e *lexError) Error() string { return e.kind.Error() + " (" + e.detail + ")" }
func (e *lexError) Unwrap() error { return e.kind }

func invalid(detail string) error { return &lexError{kind: ErrInvalidPath, detail: detail} }
func escapes(detail string) error { return &lexError{kind: ErrPathEscape, detail: detail} }

// Clean validates a path as text and returns its canonical form: components
// joined by single slashes, with no ".", no empty component and no trailing
// slash. The checkout itself is ".". The error wraps ErrInvalidPath or
// ErrPathEscape.
//
// Clean is not the security boundary. That is os.Root. Clean exists to give
// clear errors and a canonical path for the protected-path rules.
func (p *Policy) Clean(path string) (string, error) { return cleanPath(path) }

// Check runs Clean and then refuses a protected path. The error for a
// protected path wraps ErrPathProtected.
func (p *Policy) Check(path string) (string, error) {
	clean, err := cleanPath(path)
	if err != nil {
		return "", err
	}
	if rule, ok := p.Protected(clean); ok {
		return "", &lexError{kind: ErrPathProtected, detail: rule}
	}
	return clean, nil
}

func cleanPath(path string) (string, error) {
	switch {
	case path == "":
		return "", invalid("the path is empty")
	case len(path) > MaxPathBytes:
		return "", invalid(fmt.Sprintf("longer than %d bytes", MaxPathBytes))
	case !utf8.ValidString(path):
		return "", invalid("not valid UTF-8")
	}
	for _, r := range path {
		switch {
		case (r >= 0x0300 && r <= 0x036f) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r):
			return "", invalid("combining marks are not allowed in path components")
		case r == 0:
			return "", invalid("holds a NUL byte")
		case r < 0x20 || r == 0x7f:
			return "", invalid("holds a control character")
		case r == '\\':
			// A backslash is a separator on Windows and the start of a UNC
			// or device path. Paths here always use "/".
			return "", invalid(`holds a backslash; use "/" between names`)
		}
	}
	switch {
	case path[0] == '/':
		return "", escapes("absolute paths are not allowed; give a path relative to the repository root")
	case len(path) >= 2 && path[1] == ':' && isASCIILetter(path[0]):
		return "", escapes("volume paths are not allowed; give a path relative to the repository root")
	}
	parts := make([]string, 0, 8)
	for name := range strings.SplitSeq(path, "/") {
		switch {
		case name == "" || name == ".":
			continue
		case name == "..":
			return "", escapes(`".." is not allowed`)
		case len(name) > MaxComponentBytes:
			return "", invalid(fmt.Sprintf("a name is longer than %d bytes", MaxComponentBytes))
		}
		parts = append(parts, name)
	}
	if len(parts) > MaxPathDepth {
		return "", invalid(fmt.Sprintf("deeper than %d levels", MaxPathDepth))
	}
	if len(parts) == 0 {
		return ".", nil
	}
	return strings.Join(parts, "/"), nil
}

// Protected reports whether a clean path is protected, and by which rule.
// The checkout root itself is never protected.
func (p *Policy) Protected(clean string) (rule string, ok bool) {
	if clean == "." || clean == "" {
		return "", false
	}
	keys := make([]string, 0, 8)
	for name := range strings.SplitSeq(clean, "/") {
		key := matchKey(name)
		switch {
		case key == ".git" || isShortGitName(key):
			return ".git", true
		case strings.HasPrefix(key, ".env"):
			return ".env*", true
		}
		keys = append(keys, key)
	}
	for _, r := range p.rules {
		if len(keys) >= len(r.keys) && equalKeys(keys[:len(r.keys)], r.keys) {
			return r.display, true
		}
	}
	return "", false
}

func equalKeys(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// matchKey reduces a file name to the form the protected-path rules compare.
// Two names a supported file system could treat as the same file get the
// same key:
//
//   - letters are case folded (macOS and Windows ignore case)
//   - code points that HFS+ ignores in names are dropped
//   - anything from a ":" on is dropped (an NTFS stream name)
//   - trailing dots and spaces are dropped (Windows ignores them)
//
// It errs on the side of protecting too much: a strange name that only
// resembles a protected one is refused, which costs nothing.
func matchKey(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if isIgnorable(r) {
			continue
		}
		b.WriteRune(foldRune(r))
	}
	key := b.String()
	if i := strings.IndexByte(key, ':'); i >= 0 {
		key = key[:i]
	}
	return strings.TrimRight(key, ". ")
}

// foldRune maps every rune of a simple case-folding orbit to one member, so
// that "K", "k" and the Kelvin sign compare equal, as do "S", "s" and the
// long s.
func foldRune(r rune) rune {
	lowest := r
	for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
		lowest = min(lowest, next)
	}
	if 'A' <= lowest && lowest <= 'Z' {
		lowest += 'a' - 'A'
	}
	return lowest
}

// isIgnorable reports the code points HFS+ skips when it compares names.
func isIgnorable(r rune) bool {
	return (r >= 0x200C && r <= 0x200F) || (r >= 0x202A && r <= 0x202E) ||
		(r >= 0x206A && r <= 0x206F) || r == 0xFEFF
}

// isShortGitName reports a Windows 8.3 short name for .git, such as GIT~1.
func isShortGitName(key string) bool {
	digits, ok := strings.CutPrefix(key, "git~")
	if !ok || digits == "" {
		return false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isASCIILetter(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}
