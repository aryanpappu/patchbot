package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aryanpappu/patchbot/internal/llm"
)

// ReadFile is the read_file tool.
type ReadFile struct{}

type readFileArgs struct {
	Path   string `json:"path"`
	Offset *int   `json:"offset"`
	Limit  *int   `json:"limit"`
}

// Spec describes read_file.
func (ReadFile) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "read_file",
		Description: "Read a text file from the repository. Returns lines prefixed with their line number and a tab. " +
			"Reads 400 lines by default; pass offset and limit to read another part of a long file. " +
			"The last line of the result says how many lines the file has and how to continue.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path relative to the repository root, for example src/main.go."},
    "offset": {"type": "integer", "minimum": 1, "description": "Line number to start at. The first line is 1."},
    "limit": {"type": "integer", "minimum": 1, "maximum": 2000, "description": "Maximum number of lines to return."}
  },
  "required": ["path"],
  "additionalProperties": false
}`),
	}
}

// Execute runs read_file.
func (ReadFile) Execute(ctx context.Context, env *Env, raw json.RawMessage) (Result, error) {
	var args readFileArgs
	if err := decodeArgs(raw, &args); err != nil {
		return failed("%v", err)
	}
	lim := env.Limits.withDefaults()
	offset, limit := 1, lim.ReadDefaultLines
	if args.Offset != nil {
		offset = *args.Offset
	}
	if args.Limit != nil {
		limit = *args.Limit
	}
	if offset < 1 || limit < 1 {
		return failed("offset and limit must be 1 or more")
	}
	limit = min(limit, lim.ReadMaxLines)

	data, err := env.WS.ReadFile(args.Path)
	if err != nil {
		return failure(ctx, err)
	}
	if isBinary(data) {
		return failed("%q is a binary file (%d bytes) and cannot be shown", args.Path, len(data))
	}
	if len(data) == 0 {
		return Result{Content: "[the file is empty]"}, nil
	}
	lines := displayLines(env, data)
	total := len(lines)
	if offset > total {
		return failed("offset %d is past the end of the file, which has %s", offset, plural(total, "line", "lines"))
	}

	var b strings.Builder
	last := offset - 1 // the last line written, 1-based
	for i := offset - 1; i < total && i < offset-1+limit; i++ {
		line := printable(lines[i])
		if len(line) > lim.MaxLineBytes {
			line = fmt.Sprintf("%s [line cut: it has %d bytes]", cutUTF8(line, lim.MaxLineBytes), len(line))
		}
		row := fmt.Sprintf("%6d\t%s\n", i+1, line)
		if b.Len()+len(row) > lim.MaxOutputBytes && last >= offset {
			break
		}
		b.WriteString(row)
		last = i + 1
	}
	if last < total {
		fmt.Fprintf(&b, "[lines %d-%d of %d; %s; continue with offset=%d]",
			offset, last, total, plural(total-last, "more line", "more lines"), last+1)
	} else {
		fmt.Fprintf(&b, "[lines %d-%d of %d; end of file]", offset, last, total)
	}
	return Result{Content: b.String()}, nil
}
