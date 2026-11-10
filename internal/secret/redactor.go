package secret

import (
	"encoding/base64"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// MinValueLen is the shortest secret value a Redactor will register. Masking
// shorter values would damage ordinary text and reveal little.
const MinValueLen = 8

const (
	// maxPatternPrefix is longer than the text any pattern can match before it
	// reaches its minimum length, so a pattern that started more than this
	// many bytes before the end of a buffer has already matched.
	maxPatternPrefix = 128
)

type pattern struct {
	re *regexp.Regexp
	// group is the submatch to mask; 0 masks the whole match.
	group int
	// pem marks the private-key pattern, which may run to the end of input.
	pem bool
}

// The patterns carry no word-boundary anchors on purpose. Text that reaches a
// redactor is often escaped, and in "\\nghp_..." or "%20ghp_..." the token
// follows a letter or digit.
var (
	patterns = []pattern{
		// GitHub personal, OAuth, user-to-server, server-to-server and refresh tokens.
		{re: regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`)},
		{re: regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`)},
		// Anthropic keys.
		{re: regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{16,}`)},
		// OpenAI keys (sk-, sk-proj-, sk-svcacct-, ...). A run of 20 characters
		// with no hyphen is required, which random keys have and hyphenated
		// names such as "task-list-for-the-nightly-job" do not.
		{re: regexp.MustCompile(`sk-[A-Za-z0-9_-]{0,40}?[A-Za-z0-9_]{20,}[A-Za-z0-9_-]*`)},
		// Google API keys.
		{re: regexp.MustCompile(`AIza[A-Za-z0-9_-]{30,}`)},
		// Authorization headers. Only the credential is masked.
		{re: regexp.MustCompile(`(?i)bearer[ \t]{1,8}([A-Za-z0-9._~+/=-]{16,})`), group: 1},
		// PEM private keys. A block with no END line is masked to the end.
		{
			re: regexp.MustCompile(`-----BEGIN [A-Z ]{0,24}PRIVATE KEY-----[\s\S]*?` +
				`(?:-----END [A-Z ]{0,24}PRIVATE KEY-----|\z)`),
			pem: true,
		},
	}
	pemEnd = regexp.MustCompile(`-----END [A-Z ]{0,24}PRIVATE KEY-----`)
)

// Redactor masks registered secret values and token-shaped strings. The zero
// value is not usable; call NewRedactor. A nil *Redactor still masks the
// token-shaped patterns. A Redactor is safe for concurrent use.
type Redactor struct {
	mu     sync.RWMutex
	values []string
	maxLen int
}

// NewRedactor returns a Redactor that masks the given values.
func NewRedactor(values ...string) *Redactor {
	r := &Redactor{}
	r.Add(values...)
	return r
}

// Add registers secret values. Values shorter than MinValueLen are ignored.
// The standard and URL-safe base64 forms of each value are registered too,
// because that is how a secret appears inside a Basic auth header.
func (r *Redactor) Add(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, value := range values {
		if len(value) < MinValueLen {
			continue
		}
		raw := []byte(value)
		for _, form := range []string{
			value,
			base64.StdEncoding.EncodeToString(raw),
			base64.RawStdEncoding.EncodeToString(raw),
			base64.URLEncoding.EncodeToString(raw),
			base64.RawURLEncoding.EncodeToString(raw),
		} {
			if slices.Contains(r.values, form) {
				continue
			}
			r.values = append(r.values, form)
			r.maxLen = max(r.maxLen, len(form))
		}
	}
}

// AddSecrets registers the values of the given secrets.
func (r *Redactor) AddSecrets(secrets ...Secret) {
	for _, s := range secrets {
		r.Add(s.Reveal())
	}
}

// Redact returns s with every secret replaced by Mask.
func (r *Redactor) Redact(s string) string {
	spans := r.find(s)
	if len(spans) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, sp := range spans {
		b.WriteString(s[last:sp.start])
		b.WriteString(Mask)
		last = sp.end
	}
	b.WriteString(s[last:])
	return b.String()
}

// RedactBytes is Redact for a byte slice. It returns a new slice.
func (r *Redactor) RedactBytes(p []byte) []byte {
	return []byte(r.Redact(string(p)))
}

// span is a half-open byte range of s that must be masked.
type span struct {
	start, end int
	// from is where the match that produced the span begins. It is before
	// start when only part of a match is masked, as with "Bearer <token>".
	from int
	// open is set when a private-key block reached the end of the input with
	// no END line.
	open bool
}

// find returns the ranges to mask, sorted and merged so that overlapping
// secrets become one range and no part of either leaks.
func (r *Redactor) find(s string) []span {
	var spans []span
	if r != nil {
		r.mu.RLock()
		for _, value := range r.values {
			for from := 0; ; {
				i := strings.Index(s[from:], value)
				if i < 0 {
					break
				}
				start := from + i
				spans = append(spans, span{start: start, end: start + len(value), from: start})
				// Step one byte, not one match, so self-overlapping
				// occurrences are all found.
				from = start + 1
			}
		}
		r.mu.RUnlock()
	}
	for _, p := range patterns {
		for _, m := range p.re.FindAllStringSubmatchIndex(s, -1) {
			start, end := m[2*p.group], m[2*p.group+1]
			if start < 0 || start == end {
				continue
			}
			open := p.pem && end == len(s) && !pemEnd.MatchString(s[start:end])
			spans = append(spans, span{start: start, end: end, from: m[0], open: open})
		}
	}
	if len(spans) < 2 {
		return spans
	}
	slices.SortFunc(spans, func(a, b span) int {
		if a.start != b.start {
			return a.start - b.start
		}
		return b.end - a.end
	})
	merged := spans[:1]
	for _, sp := range spans[1:] {
		last := &merged[len(merged)-1]
		if sp.start > last.end {
			merged = append(merged, sp)
			continue
		}
		last.from = min(last.from, sp.from)
		if sp.end > last.end {
			last.end = sp.end
			last.open = sp.open
		} else if sp.end == last.end {
			last.open = last.open || sp.open
		}
	}
	return merged
}

// overlap is how many trailing bytes a stream must hold back so that a secret
// split across two writes is still seen whole.
func (r *Redactor) overlap() int {
	n := maxPatternPrefix
	if r != nil {
		r.mu.RLock()
		n = max(n, r.maxLen)
		r.mu.RUnlock()
	}
	return n
}
