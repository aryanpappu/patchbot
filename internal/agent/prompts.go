package agent

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/aryanpappu/patchbot/internal/llm"
)

//go:embed prompts/refine.txt
var refinePrompt string

//go:embed prompts/code.txt
var codePrompt string

//go:embed prompts/investigate.txt
var investigatePrompt string

// IssueHash binds an approval to the exact title and body, including whitespace.
// JSON array encoding makes the boundary between the two fields unambiguous.
func IssueHash(title, body string) string {
	raw, _ := json.Marshal([2]string{title, body})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func stageContext(ctx context.Context, deadline, now time.Time) (context.Context, context.CancelFunc) {
	if deadline.IsZero() {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, max(time.Duration(0), deadline.Sub(now)))
}

func startStage(s *Session, system, task string) *Session {
	if s == nil {
		s = &Session{}
	}
	s.System, s.Messages = system, []llm.Message{llm.UserText(task)}
	// Limits and budget charges survive stage transitions; conversation heuristics
	// belong to the stage. Test repair does not call startStage.
	s.endTurns, s.malformed, s.stuckCount, s.denials = 0, 0, 0, 0
	s.contextCompacted = false
	s.turnBase = s.Turns
	s.deadline = time.Time{}
	s.LastInputTokens = 0
	return s
}

func issueData(title, body string) string {
	return untrusted("issue title", title) + "\n" + untrusted("issue body", body)
}
