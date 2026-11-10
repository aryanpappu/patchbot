package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/workspace"
)

// EditFile is the edit_file tool.
type EditFile struct{}

type editFileArgs struct {
	Path       string  `json:"path"`
	OldString  *string `json:"old_string"`
	NewString  *string `json:"new_string"`
	ReplaceAll bool    `json:"replace_all"`
}

// Spec describes edit_file.
func (EditFile) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "edit_file",
		Description: "Replace text in an existing file. old_string must match the file exactly, including " +
			"indentation and line breaks, and must occur exactly once: include enough surrounding lines to make " +
			"it unique. If it occurs more than once the call fails and says how many times it matched. " +
			"Set replace_all to replace every occurrence. Do not include the line-number prefixes of read_file.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path relative to the repository root."},
    "old_string": {"type": "string", "description": "The exact text to replace."},
    "new_string": {"type": "string", "description": "The text to put in its place. May be empty to delete old_string."},
    "replace_all": {"type": "boolean", "description": "Replace every occurrence instead of requiring exactly one. The default is false."}
  },
  "required": ["path", "old_string", "new_string"],
  "additionalProperties": false
}`),
	}
}

// Execute runs edit_file.
func (EditFile) Execute(ctx context.Context, env *Env, raw json.RawMessage) (Result, error) {
	var args editFileArgs
	if err := decodeArgs(raw, &args); err != nil {
		return failed("%v", err)
	}
	switch {
	case args.OldString == nil || args.NewString == nil:
		return failed("old_string and new_string are required")
	case *args.OldString == "":
		return failed("old_string is empty; to create a file or replace all of it, use write_file")
	case *args.OldString == *args.NewString:
		return failed("old_string and new_string are the same, so there is nothing to change")
	}
	data, err := env.WS.ReadFile(args.Path)
	if err != nil {
		return failure(ctx, err)
	}
	if isBinary(data) {
		return failed("%q is a binary file and cannot be edited", args.Path)
	}
	content, oldText, newText := string(data), *args.OldString, *args.NewString
	count := strings.Count(content, oldText)
	crlf := false
	if count == 0 && strings.Contains(content, "\r\n") && !strings.Contains(oldText, "\r") {
		// read_file shows the lines of a CRLF file without the carriage
		// returns, so text copied from it has plain line feeds. Match it
		// against the file's own line endings.
		// Do not expand either argument until its size has been checked.
		if strings.Count(oldText, "\n") > len(content)-len(oldText) {
			return failed("old_string was not found in %q (0 matches); nothing was changed", args.Path)
		}
		oldText = strings.ReplaceAll(oldText, "\n", "\r\n")
		crlf = true
		count = strings.Count(content, oldText)
	}
	switch {
	case count == 0:
		return failed("old_string was not found in %q (0 matches); nothing was changed. "+
			"Read the file again and copy the text exactly, including whitespace", args.Path)
	case count > 1 && !args.ReplaceAll:
		return failed("old_string matches %d times in %q; nothing was changed. "+
			"Add surrounding lines to make it match exactly once, or set replace_all", count, args.Path)
	}
	limit := int64(workspace.DefaultMaxWriteBytes)
	if ws, ok := env.WS.(interface{ MaxWriteBytes() int64 }); ok {
		limit = ws.MaxWriteBytes()
	}
	newLen := int64(len(newText))
	if crlf {
		extra := int64(strings.Count(newText, "\n") - strings.Count(newText, "\r\n"))
		if newLen > int64(^uint64(0)>>1)-extra {
			return failed("edit result exceeds the write size limit")
		}
		newLen += extra
	}
	if !replacementFits(int64(len(content)), int64(len(oldText)), newLen, int64(count), limit) {
		return failed("edit result exceeds the write size limit of %d bytes", limit)
	}
	if crlf {
		newText = strings.ReplaceAll(strings.ReplaceAll(newText, "\r\n", "\n"), "\n", "\r\n")
	}
	before, _, _ := strings.Cut(content, oldText)
	line := 1 + strings.Count(before, "\n")
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := env.WS.WriteFile(args.Path, []byte(strings.ReplaceAll(content, oldText, newText))); err != nil {
		return failure(ctx, err)
	}
	where := fmt.Sprintf("at line %d", line)
	if count > 1 {
		where = fmt.Sprintf("the first at line %d", line)
	}
	return Result{Content: fmt.Sprintf("edited %s: replaced %s, %s", args.Path, plural(count, "occurrence", "occurrences"), where)}, nil
}

// replacementFits checks the final size without multiplying untrusted lengths
// until the division proves that the product fits both the limit and an int.
func replacementFits(size, oldLen, newLen, count, limit int64) bool {
	if size < 0 || oldLen <= 0 || newLen < 0 || count <= 0 || count > size/oldLen || limit < 0 {
		return false
	}
	limit = min(limit, int64(int(^uint(0)>>1)))
	remaining := size - count*oldLen
	return remaining <= limit && newLen <= (limit-remaining)/count
}
