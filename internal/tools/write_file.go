package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"

	"github.com/aryanpappu/patchbot/internal/llm"
)

// WriteFile is the write_file tool.
type WriteFile struct{}

type writeFileArgs struct {
	Path    string  `json:"path"`
	Content *string `json:"content"`
}

// Spec describes write_file.
func (WriteFile) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "write_file",
		Description: "Create a file, or replace the whole content of an existing one. Missing parent directories " +
			"are created. To change part of an existing file, use edit_file instead. The limit is 1 MB.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path relative to the repository root."},
    "content": {"type": "string", "description": "The full new content of the file."}
  },
  "required": ["path", "content"],
  "additionalProperties": false
}`),
	}
}

// Execute runs write_file.
func (WriteFile) Execute(ctx context.Context, env *Env, raw json.RawMessage) (Result, error) {
	var args writeFileArgs
	if err := decodeArgs(raw, &args); err != nil {
		return failed("%v", err)
	}
	if args.Content == nil {
		return failed("content is required")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	verb := "replaced"
	if _, err := env.WS.Stat(args.Path); errors.Is(err, fs.ErrNotExist) {
		verb = "created"
	}
	if err := env.WS.WriteFile(args.Path, []byte(*args.Content)); err != nil {
		return failure(ctx, err)
	}
	return Result{Content: verb + " " + args.Path + " (" + plural(len(*args.Content), "byte", "bytes") + ")"}, nil
}
