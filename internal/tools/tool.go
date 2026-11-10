// Package tools holds the tools the model can call and the registry that
// dispatches its calls. The file tools here reach the checkout only through
// a workspace.Workspace, so the path jail applies to all of them alike.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/workspace"
)

// Tool is one function the model can call.
type Tool interface {
	// Spec describes the tool to the model.
	Spec() llm.ToolSpec
	// Execute runs the tool. The arguments have already passed the schema
	// of Spec. It returns a Result for anything the model can react to,
	// including its own mistakes. A non-nil error means the harness itself
	// cannot go on, such as a cancelled context, and the run must stop.
	Execute(ctx context.Context, env *Env, args json.RawMessage) (Result, error)
}

// Result is what a tool hands back to the model.
type Result struct {
	Content string
	// IsError marks a failure the model should read and react to.
	IsError bool
}

// Env is what a tool may use.
type Env struct {
	// WS is the jailed checkout. Required.
	WS workspace.Workspace
	// Limits caps what the tools read and return. Zero fields take the
	// defaults.
	Limits Limits
	// Redactor masks secrets in every result before the model sees it. A
	// nil Redactor still masks token-shaped strings.
	Redactor *secret.Redactor
	// Log receives one debug record per call. It may be nil.
	Log *slog.Logger
}

// Limits caps what the file tools read and return.
type Limits struct {
	// ReadDefaultLines and ReadMaxLines bound the lines of one read_file.
	ReadDefaultLines int
	ReadMaxLines     int
	// MaxLineBytes is the longest line read_file shows before it cuts it.
	MaxLineBytes int
	// MaxOutputBytes bounds the result of read_file, list_dir and search.
	MaxOutputBytes int

	// ListMaxDepth and ListMaxEntries bound list_dir.
	ListMaxDepth   int
	ListMaxEntries int

	// SearchDefaultResults and SearchMaxResults bound the matches of one
	// search.
	SearchDefaultResults int
	SearchMaxResults     int
	// SearchMaxFiles is the most files one search reads.
	SearchMaxFiles int
	// SearchMaxDirs is the most directories one search lists.
	SearchMaxDirs int
	// SearchMaxBytes is the most file content one search reads in total.
	SearchMaxBytes int64
	// SearchMaxFileBytes is the largest file search reads; larger ones are
	// skipped.
	SearchMaxFileBytes int64
	// SearchMaxLineBytes is the longest matching line shown before it is
	// cut.
	SearchMaxLineBytes int
}

// DefaultLimits returns the limits used when Env.Limits is zero.
func DefaultLimits() Limits {
	return Limits{
		ReadDefaultLines:     400,
		ReadMaxLines:         2000,
		MaxLineBytes:         2000,
		MaxOutputBytes:       64 << 10,
		ListMaxDepth:         3,
		ListMaxEntries:       500,
		SearchDefaultResults: 50,
		SearchMaxResults:     200,
		SearchMaxFiles:       5000,
		SearchMaxDirs:        5000,
		SearchMaxBytes:       32 << 20,
		SearchMaxFileBytes:   1 << 20,
		SearchMaxLineBytes:   300,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	pick := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	pick64 := func(v *int64, def int64) {
		if *v <= 0 {
			*v = def
		}
	}
	pick(&l.ReadDefaultLines, d.ReadDefaultLines)
	pick(&l.ReadMaxLines, d.ReadMaxLines)
	pick(&l.MaxLineBytes, d.MaxLineBytes)
	pick(&l.MaxOutputBytes, d.MaxOutputBytes)
	pick(&l.ListMaxDepth, d.ListMaxDepth)
	pick(&l.ListMaxEntries, d.ListMaxEntries)
	pick(&l.SearchDefaultResults, d.SearchDefaultResults)
	pick(&l.SearchMaxResults, d.SearchMaxResults)
	pick(&l.SearchMaxFiles, d.SearchMaxFiles)
	pick(&l.SearchMaxDirs, d.SearchMaxDirs)
	pick64(&l.SearchMaxBytes, d.SearchMaxBytes)
	pick64(&l.SearchMaxFileBytes, d.SearchMaxFileBytes)
	pick(&l.SearchMaxLineBytes, d.SearchMaxLineBytes)
	return l
}

// binarySniffBytes is how much of a file is checked for a NUL byte to decide
// that it is binary.
const binarySniffBytes = 8 << 10

// skippedDirs are directory names a recursive list or search does not go
// into, because they hold installed dependencies, not the project's own
// code, and would use up the caps. Naming such a directory as the starting
// path still works.
var skippedDirs = map[string]bool{"node_modules": true}

func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), binarySniffBytes)], 0) >= 0
}

// decodeArgs decodes tool arguments into a struct and refuses unknown
// fields.
func decodeArgs(args json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// failed returns the Result for a mistake the model can correct.
func failed(format string, args ...any) (Result, error) {
	return Result{Content: "error: " + fmt.Sprintf(format, args...), IsError: true}, nil
}

// failure sorts an error from the workspace or the context. A cancelled or
// expired context stops the run. Everything else is reported to the model.
func failure(ctx context.Context, err error) (Result, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Result{}, ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Result{}, err
	}
	return failed("%s", describe(err))
}

// describe renders a workspace error for the model. A *workspace.PathError
// never holds a host path. Any other error is not expected here, so its
// text is withheld in case it does.
func describe(err error) string {
	var pe *workspace.PathError
	if errors.As(err, &pe) {
		return pe.Error()
	}
	return "the file operation failed"
}

// relabel names the tool's own operation in a workspace error, in place of
// the "stat" of the check that found it.
func relabel(err error, op string) error {
	var pe *workspace.PathError
	if errors.As(err, &pe) {
		return &workspace.PathError{Op: op, Path: pe.Path, Err: pe.Err, Detail: pe.Detail}
	}
	return err
}

// cutUTF8 shortens s to at most limit bytes without splitting a character.
func cutUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

// printable makes a line of file content safe to put in a result: valid
// UTF-8, no trailing carriage return.
func printable(line string) string {
	return strings.ToValidUTF8(strings.TrimSuffix(line, "\r"), "�")
}

// plural returns "1 match" or "3 matches".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
