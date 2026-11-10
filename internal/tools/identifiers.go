package tools

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aryanpappu/patchbot/internal/llm"
)

var toolNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidateToolIdentifiers rejects unsafe metadata before dispatch or replay.
// IDs allow provider-specific punctuation, but are limited to 256 UTF-8 bytes
// without control characters. Errors never echo model-controlled identifiers.
func ValidateToolIdentifiers(call llm.ToolCall) error {
	if !toolNameRE.MatchString(call.Name) {
		return errors.New("invalid tool name")
	}
	if call.ID == "" || len(call.ID) > 256 || !utf8.ValidString(call.ID) || strings.ContainsFunc(call.ID, unicode.IsControl) {
		return errors.New("invalid tool call ID")
	}
	return nil
}
