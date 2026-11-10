// Package withouttoken is a negative compilation fixture, excluded from ./....
package withouttoken

import (
	"context"
	"github.com/aryanpappu/patchbot/internal/agent"
)

func cannotCallWithoutToken(l *agent.Loop) {
	_, _ = l.Code(context.Background(), agent.CodeRequest{})
	_, _ = l.Code(context.Background(), nil, agent.CodeRequest{})
	_, _ = l.Code(context.Background(), forged{}, agent.CodeRequest{})
}

type forged struct{}

// The valid entry remains callable from an external package through inference.
func checked(l *agent.Loop, s agent.ApprovalStore) {
	ctx := context.Background()
	token, err := agent.Check(ctx, s, 1, "issue")
	if err != nil {
		return
	}
	_, _ = l.Code(ctx, token, agent.CodeRequest{})
}
