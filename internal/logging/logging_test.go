package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/logging"
	"github.com/aryanpappu/patchbot/internal/secret"
)

const (
	known = "known-secret-value-42"
	token = "ghp_" + "0123456789abcdefghijABCDEFGHIJ012345"
)

type stringer struct{ s string }

func (s stringer) String() string { return s.s }

type nilStringer struct{ s string }

func (n *nilStringer) String() string { return n.s }

type payload struct {
	Name string
	Auth string
}

func TestLoggerRedacts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		log  func(l *slog.Logger)
	}{
		{"message", func(l *slog.Logger) { l.Info("push with " + token + " and " + known) }},
		{"string attr", func(l *slog.Logger) { l.Info("m", "a", known, "b", token) }},
		{"attr key", func(l *slog.Logger) { l.Info("m", known, 1) }},
		{"error attr", func(l *slog.Logger) { l.Error("m", "err", fmt.Errorf("auth failed for %s", known)) }},
		{"wrapped error", func(l *slog.Logger) { l.Error("m", "err", fmt.Errorf("outer: %w", errors.New(token))) }},
		{"stringer attr", func(l *slog.Logger) { l.Info("m", "s", stringer{known}) }},
		{"byte slice attr", func(l *slog.Logger) { l.Info("m", "b", []byte(known)) }},
		{"struct attr", func(l *slog.Logger) { l.Info("m", "p", payload{Name: "x", Auth: known}) }},
		{"pointer to struct attr", func(l *slog.Logger) { l.Info("m", "p", &payload{Name: "x", Auth: token}) }},
		{"map attr", func(l *slog.Logger) { l.Info("m", "h", map[string]string{"Authorization": "Bearer " + known}) }},
		{"slice attr", func(l *slog.Logger) { l.Info("m", "argv", []string{"git", "push", known}) }},
		{"group attr", func(l *slog.Logger) {
			l.Info("m", slog.Group("g", slog.String("k", known), slog.Group("h", slog.Any("e", errors.New(token)))))
		}},
		{"log valuer", func(l *slog.Logger) { l.Info("m", "v", valuer{known}) }},
		{"with attrs", func(l *slog.Logger) { l.With("k", known, "t", token).Info("m") }},
		{"with group", func(l *slog.Logger) { l.WithGroup("g").With("k", known).Info("m", "t", token) }},
		{"group name", func(l *slog.Logger) { l.WithGroup(known).Info("m", "a", 1) }},
		{"with run", func(l *slog.Logger) { logging.WithRun(l, known).Info("m") }},
		{"secret type", func(l *slog.Logger) { l.Info("m", "key", secret.New(known)) }},
		{"log attrs", func(l *slog.Logger) {
			l.LogAttrs(context.Background(), slog.LevelWarn, known, slog.String("k", token))
		}},
	}
	for _, format := range []string{"text", "json"} {
		for _, tt := range tests {
			t.Run(format+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				var buf bytes.Buffer
				l, err := logging.New(&buf, logging.Options{Format: format}, secret.NewRedactor(known))
				if err != nil {
					t.Fatal(err)
				}
				tt.log(l)
				got := buf.String()
				if got == "" {
					t.Fatal("nothing was logged")
				}
				if strings.Contains(got, known) || strings.Contains(got, token) {
					t.Errorf("log line leaks a secret: %s", got)
				}
				if !strings.Contains(got, secret.Mask) {
					t.Errorf("log line has no %s: %s", secret.Mask, got)
				}
			})
		}
	}
}

type valuer struct{ s string }

func (v valuer) LogValue() slog.Value { return slog.StringValue(v.s) }

func TestLoggerKeepsOrdinaryValues(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l, err := logging.New(&buf, logging.Options{Format: "json", Level: "debug"}, secret.NewRedactor(known))
	if err != nil {
		t.Fatal(err)
	}
	var nilPtr *nilStringer
	logging.WithRun(l, "run-7").Debug("step done",
		"turn", 3, "cost", 1.5, "ok", true, "took", 2*time.Second, "tool", "read_file",
		"nil", nil, "nilptr", nilPtr, slog.Group("usage", slog.Int64("input", 120)))

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not JSON: %v: %s", err, buf.String())
	}
	usage, _ := rec["usage"].(map[string]any)
	for key, want := range map[string]any{
		"msg": "step done", "level": "DEBUG", "run_id": "run-7", "turn": 3.0, "cost": 1.5, "ok": true,
		"took": float64(2 * time.Second), "tool": "read_file", "nil": "<nil>", "nilptr": "<nil>",
	} {
		if rec[key] != want {
			t.Errorf("%s = %#v, want %#v", key, rec[key], want)
		}
	}
	if usage["input"] != 120.0 {
		t.Errorf("usage.input = %#v, want 120", usage["input"])
	}
}

func TestLevels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		level     string
		wantLines int
		wantErr   bool
	}{
		{"debug", 4, false},
		{"", 3, false},
		{"info", 3, false},
		{"WARN", 2, false},
		{"warning", 2, false},
		{"error", 1, false},
		{"loud", 0, true},
	}
	for _, tt := range tests {
		t.Run("level "+tt.level, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			l, err := logging.New(&buf, logging.Options{Level: tt.level}, nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("New error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			l.Debug("d")
			l.Info("i")
			l.Warn("w")
			l.Error("e")
			if got := strings.Count(buf.String(), "\n"); got != tt.wantLines {
				t.Errorf("%d lines logged, want %d", got, tt.wantLines)
			}
		})
	}
}

func TestBadFormat(t *testing.T) {
	t.Parallel()
	if _, err := logging.New(&bytes.Buffer{}, logging.Options{Format: "xml"}, nil); err == nil {
		t.Error("New accepted the format xml")
	}
}

func TestNilRedactorStillMasksTokenShapes(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l, err := logging.New(&buf, logging.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Info("clone", "header", "Authorization: Bearer "+token)
	if strings.Contains(buf.String(), token) {
		t.Errorf("log line leaks the token: %s", buf.String())
	}
}

// TestSecretRegisteredAfterBinding covers a logger that was given attributes
// and groups before the secret was known, which is the normal start-up order:
// the logger is built first and the API keys are read later.
func TestSecretRegisteredAfterBinding(t *testing.T) {
	t.Parallel()
	const late = "late-secret-value-777"
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			r := secret.NewRedactor()
			base, err := logging.New(&buf, logging.Options{Format: format}, r)
			if err != nil {
				t.Fatal(err)
			}
			bound := base.With("auth", late, slog.Group("req", slog.String("header", "x "+late))).
				WithGroup(late).
				With("inner", errors.New("failed with "+late))
			sibling := base.With("other", "plain")

			r.Add(late) // registered only now

			bound.Info("first", "k", late)
			bound.Info("second")
			sibling.Info("third", "k", late)
			got := buf.String()
			if strings.Contains(got, late) {
				t.Errorf("a value bound before its secret was registered is logged:\n%s", got)
			}
			if strings.Count(got, "\n") != 3 || !strings.Contains(got, "other=plain") && !strings.Contains(got, `"other":"plain"`) {
				t.Errorf("records or ordinary bound attributes were lost:\n%s", got)
			}
		})
	}
}

func TestBindingKeepsOrderAndIsolation(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	base, err := logging.New(&buf, logging.Options{Format: "json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := base.With("a", 1).WithGroup("g").With("b", 2)
	// Two children of one parent must not see each other's attributes.
	left, right := parent.With("side", "left"), parent.With("side", "right")
	left.Info("l", "c", 3)
	right.WithGroup("").With().Info("r")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2:\n%s", len(lines), buf.String())
	}
	var l, r map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &l); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &r); err != nil {
		t.Fatal(err)
	}
	lg, _ := l["g"].(map[string]any)
	rg, _ := r["g"].(map[string]any)
	if l["a"] != 1.0 || lg["b"] != 2.0 || lg["side"] != "left" || lg["c"] != 3.0 {
		t.Errorf("left record = %v", l)
	}
	if r["a"] != 1.0 || rg["b"] != 2.0 || rg["side"] != "right" {
		t.Errorf("right record = %v", r)
	}
}
