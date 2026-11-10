package tools

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/workspace"
)

type editProbe struct {
	workspace.Workspace
	data  []byte
	limit int64
	wrote bool
}

func (p *editProbe) ReadFile(string) ([]byte, error) { return p.data, nil }
func (p *editProbe) MaxWriteBytes() int64            { return p.limit }
func (p *editProbe) WriteFile(string, []byte) error {
	p.wrote = true
	return nil
}

// Finding 2: bounded reproductions of the multi-terabyte expansion. The probe
// proves rejection occurs in the tool, before the workspace ever sees a result.
func TestEditRejectsExpansionBeforeWrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, content, old, replacement string
		limit                           int64
	}{
		{"replace all", strings.Repeat("a", 4096), "a", strings.Repeat("b", 1024), 1 << 20},
		{"CRLF expansion", "a\r\n", "a\n", strings.Repeat("\n", 9), 12},
		{"CRLF old expansion", "a\r\n", strings.Repeat("a\n", 100), "b", 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &editProbe{data: []byte(tc.content), limit: tc.limit}
			raw, err := json.Marshal(map[string]any{"path": "file", "old_string": tc.old, "new_string": tc.replacement, "replace_all": true})
			if err != nil {
				t.Fatal(err)
			}
			res, err := (EditFile{}).Execute(t.Context(), &Env{WS: probe}, raw)
			if err != nil || !res.IsError || probe.wrote {
				t.Fatalf("Execute = %+v, %v, wrote=%v", res, err, probe.wrote)
			}
		})
	}
}

func TestReplacementSizeCheckedArithmetic(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		size, oldLen, newLen, count, limit int64
		want                               bool
	}{
		{4 << 20, 1, 1 << 20, 4 << 20, 1 << 20, false}, // original 4 TiB case
		{math.MaxInt64, 1, math.MaxInt64, math.MaxInt64, math.MaxInt64, false},
		{10, 1, 0, 10, 0, true},
		{10, 2, 4, 2, 14, true},
		{10, 2, 4, 2, 13, false},
		{10, 2, 0, 6, 10, false},
	} {
		if got := replacementFits(tc.size, tc.oldLen, tc.newLen, tc.count, tc.limit); got != tc.want {
			t.Errorf("replacementFits(%+v) = %v", tc, got)
		}
	}
}

// A deterministic context seam that expires after the file-level check. A
// regressed search with no per-line checks would return successful results.
type lineCancelContext struct {
	context.Context
	checks int
}

func (c *lineCancelContext) Err() error {
	c.checks++
	if c.checks > 2 {
		return context.Canceled
	}
	return nil
}

func TestSearchChecksCancellationBetweenLines(t *testing.T) {
	t.Parallel()
	probe := &editProbe{data: []byte("a\nb\nc\n")}
	ctx := &lineCancelContext{Context: t.Context()}
	s := searcher{ctx: ctx, env: &Env{WS: probe}, lim: DefaultLimits(), re: regexp.MustCompile("never"), maxResults: 50}
	if err := s.file("file", int64(len(probe.data))); !errors.Is(err, context.Canceled) {
		t.Fatalf("search ignored cancellation between lines: %v", err)
	}
}
