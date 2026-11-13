package tools

import (
	"context"
	"encoding/json"
	"errors"
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
	text = strings.TrimSpace(redactor.Redact(strings.ToValidUTF8(text, "�")))
	if text == "" || len(text) > MaxLessonBytes || !utf8.ValidString(text) {
		return "", ErrInvalidLesson
	}
	if strings.ContainsFunc(text, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		return "", ErrInvalidLesson
	}
	return text, nil
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
