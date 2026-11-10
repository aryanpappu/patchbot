// Package registryinjection documents forbidden public registry injection.
package registryinjection

import (
	"context"

	"github.com/aryanpappu/patchbot/internal/agent"
	"github.com/aryanpappu/patchbot/internal/tools"
)

func cannotInjectRegistry(loop *agent.Loop, env *tools.Env) {
	registry, _ := tools.NewCodeRegistry(env, tools.RunCmd{})
	loop.Tools = registry
	_ = agent.Loop{Tools: registry}
	_, _ = loop.Code(context.Background(), registry, agent.CodeRequest{})
}
