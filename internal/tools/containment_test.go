package tools_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/execx"
	"github.com/aryanpappu/patchbot/internal/tools"
)

func TestExecuteCommandSurfacesContainmentFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{"workspace limit", errors.Join(errors.New("runtime said /work/secret"), execx.ErrWorkspaceLimit), execx.ErrWorkspaceLimit},
		{"container not removed", execx.ErrContainerNotRemoved, execx.ErrContainerNotRemoved},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			env := commandEnv(t)
			env.Exec = &commandExecutor{err: test.err}
			_, err := tools.ExecuteCommand(t.Context(), env, "test", nil, 0)
			if !errors.Is(err, test.want) || strings.Contains(err.Error(), "/work/secret") {
				t.Fatalf("ExecuteCommand error = %v; want %v without runtime text", err, test.want)
			}
		})
	}
}
