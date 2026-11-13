package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/secret"
)

// Lesson limits shared by the tool and operator input.
const (
	MaxLessonBytes   = 600
	MaxLessonsPerRun = 3
)

// ErrInvalidLesson identifies rejected lesson text without quoting it.
var ErrInvalidLesson = errors.New("lesson must be 1 to 600 bytes of printable text")

// CleanLesson trims, redacts and bounds lesson text. Redaction happens before
// the length check so the stored text is exactly what was validated.
func CleanLesson(text string, redactor *secret.Redactor) (string, error) {
	text = strings.TrimSpace(redactor.Redact(strings.ToValidUTF8(text, "\uFFFD")))
	if text == "" || len(text) > MaxLessonBytes || !utf8.ValidString(text) {
		return "", ErrInvalidLesson
	}
	if strings.ContainsFunc(text, func(r rune) bool { return !lessonRune(r) }) {
		return "", ErrInvalidLesson
	}
	return text, nil
}

// lessonRune allows printable runes (letters, marks, numbers, punctuation,
// symbols and ASCII space) plus newline and tab. Everything else is refused,
// including format characters (zero-width, bidi overrides U+202A-202E and
// U+2066-2069, tag characters U+E0000-E007F), private use, surrogates and
// line/paragraph separators, which can hide text from a reviewer.
func lessonRune(r rune) bool {
	return r == '\n' || r == '\t' || unicode.IsPrint(r)
}

// Visible escapes every rune a lesson could not contain, so legacy or
// operator rows can never hide text from a reviewer. Newline and tab remain.
func Visible(text string) string {
	if !strings.ContainsFunc(text, func(r rune) bool { return !lessonRune(r) }) {
		return text
	}
	var b strings.Builder
	for _, r := range text {
		if lessonRune(r) {
			b.WriteRune(r)
		} else {
			fmt.Fprintf(&b, "\\u{%X}", r)
		}
	}
	return b.String()
}

// LessonBox collects a stage's lesson proposals. It is created by the harness,
// read after the stage, and never consulted by any policy check.
type LessonBox struct {
	mu      sync.Mutex
	lessons []string
}

// Lessons returns a copy of the accepted proposals.
func (b *LessonBox) Lessons() []string {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.lessons...)
}

func (b *LessonBox) add(text string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.lessons) >= MaxLessonsPerRun {
		return false
	}
	b.lessons = append(b.lessons, text)
	return true
}

// ProposeLesson lets the model suggest a reusable lesson for future runs. A
// proposal is stored as pending; it affects nothing until an operator approves
// it, and even then it is delivered only as untrusted context.
type ProposeLesson struct{ Box *LessonBox }

// Spec describes propose_lesson.
func (ProposeLesson) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "propose_lesson", Description: "Optionally propose one short, reusable lesson about this repository for future runs (at most 3 per run, 600 bytes each). An operator must approve it before any future run sees it. It has no effect on this run, its permissions or its scope.", Schema: json.RawMessage(`{"type":"object","properties":{"lesson":{"type":"string","description":"A concise, factual lesson. Never include secrets."}},"required":["lesson"],"additionalProperties":false}`)}
}

// Execute validates and records a proposal.
func (t ProposeLesson) Execute(ctx context.Context, env *Env, raw json.RawMessage) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if t.Box == nil {
		return failed("lesson proposals are disabled")
	}
	var args struct {
		Lesson string `json:"lesson"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return failed("%v", err)
	}
	text, err := CleanLesson(args.Lesson, env.Redactor)
	if err != nil {
		return failed("%v", err)
	}
	if !t.Box.add(text) {
		return failed("lesson limit reached for this run")
	}
	return Result{Content: "Lesson recorded as pending operator review. It has no effect on this run."}, nil
}
