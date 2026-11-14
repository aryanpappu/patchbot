package runner

import (
	"context"
	"strings"
)

type actorKey struct{}

// WithActor makes operator actions on ctx record actor instead of
// approval.local_actor. The web server sets it to the verified login email.
func WithActor(ctx context.Context, actor string) context.Context {
	if strings.TrimSpace(actor) == "" {
		return ctx
	}
	return context.WithValue(ctx, actorKey{}, actor)
}

// actorFor returns the context actor or the configured local actor.
func (r *Runner) actorFor(ctx context.Context) string {
	if actor, ok := ctx.Value(actorKey{}).(string); ok && actor != "" {
		return actor
	}
	return r.cfg.Approval.LocalActor
}
