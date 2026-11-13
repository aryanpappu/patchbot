package github

import (
	"regexp"
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

// markerPattern matches any patchbot HTML-comment marker, tolerating case and
// whitespace variations, so a near-miss spelling still fails closed.
var markerPattern = regexp.MustCompile(`(?i)<!--\s*patchbot`)

// HasPatchbotMarker reports whether a body carries a hidden patchbot marker.
// Every comment patchbot posts carries one (see Marker).
func HasPatchbotMarker(body string) bool { return markerPattern.MatchString(body) }

// Marker is the hidden marker appended to a patchbot comment of the given kind.
func Marker(kind string) string { return "<!-- patchbot:" + kind + " -->" }

// EligibleCommand checks identity, edit status and live collaborator membership.
// The runner supplies its own login explicitly; an unknown bot identity fails
// closed. The callback can adapt Client.IsCollaborator and must return false on
// error. Run status, spec integrity and comment ordering belong to the runner.
//
// Shared identity: when the token owner is also a configured approver (a
// single-user PAT), a comment authored by botLogin is eligible only if it is
// not one patchbot posted. postedByPatchbot must report recorded patchbot
// comment IDs and return true on error. Such a comment must also carry no
// patchbot marker, be unedited and pass the approver and collaborator checks.
// Every other combination fails closed. Other authors keep the plain rules.
func EligibleCommand(comment Comment, approvers []string, isCollaborator func(string) bool, botLogin string, postedByPatchbot func(int64) bool) bool {
	if botLogin == "" || comment.Author == "" || comment.CreatedAt.IsZero() ||
		!comment.UpdatedAt.Equal(comment.CreatedAt) || isCollaborator == nil {
		return false
	}
	if strings.EqualFold(comment.Author, botLogin) &&
		(comment.ID <= 0 || HasPatchbotMarker(comment.Body) || postedByPatchbot == nil || postedByPatchbot(comment.ID)) {
		return false
	}
	for _, approver := range approvers {
		if strings.EqualFold(approver, comment.Author) {
			return isCollaborator(comment.Author)
		}
	}
	return false
}
