package github

import (
	"strings"
	"unicode"
)

// CommandKind names a comment action. Authorization is checked separately.
type CommandKind string

// Supported comment actions.
const (
	CommandApprove CommandKind = "approve"
	CommandReject  CommandKind = "reject"
	CommandRevise  CommandKind = "revise"
	CommandAnswer  CommandKind = "answer"
)

// Command is a parsed action with optional multiline notes.
type Command struct {
	Kind  CommandKind
	Notes string
}

// ParseCommand recognizes an action only on the first nonempty line. A quoted
// or fenced first line is not a command. Approve and reject take no inline
// arguments; revise and answer require text, which may start on the next line.
func ParseCommand(comment Comment) (Command, bool) {
	body := strings.TrimSpace(strings.ReplaceAll(comment.Body, "\r\n", "\n"))
	first, rest, _ := strings.Cut(body, "\n")
	first = strings.TrimSpace(first)
	name, inline := first, ""
	if index := strings.IndexFunc(first, unicode.IsSpace); index >= 0 {
		name, inline = first[:index], strings.TrimSpace(first[index:])
	}
	var kind CommandKind
	switch strings.ToLower(name) {
	case "/approve":
		kind = CommandApprove
	case "/reject":
		kind = CommandReject
	case "/revise":
		kind = CommandRevise
	case "/answer":
		kind = CommandAnswer
	default:
		return Command{}, false
	}
	if (kind == CommandApprove || kind == CommandReject) && inline != "" {
		return Command{}, false
	}
	notes := strings.TrimSpace(inline + "\n" + rest)
	if (kind == CommandRevise || kind == CommandAnswer) && notes == "" {
		return Command{}, false
	}
	return Command{Kind: kind, Notes: notes}, true
}

// EligibleCommand checks identity, edit status and live collaborator membership.
// The runner supplies its own login explicitly; an unknown bot identity fails
// closed. The callback can adapt Client.IsCollaborator and must return false on
// error. Run status, spec integrity and comment ordering belong to the runner.
func EligibleCommand(comment Comment, approvers []string, isCollaborator func(string) bool, botLogin string) bool {
	if botLogin == "" || comment.Author == "" || strings.EqualFold(comment.Author, botLogin) ||
		comment.CreatedAt.IsZero() || !comment.UpdatedAt.Equal(comment.CreatedAt) || isCollaborator == nil {
		return false
	}
	for _, approver := range approvers {
		if strings.EqualFold(approver, comment.Author) {
			return isCollaborator(comment.Author)
		}
	}
	return false
}
