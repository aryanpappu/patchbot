package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/agent"
	"github.com/aryanpappu/patchbot/internal/execx"
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
	"github.com/aryanpappu/patchbot/internal/workspace"
)

// ExampleLoop_Code documents the public capability API. The inferred private
// token is accepted; a registry, nil, or a caller-defined token is not.
func ExampleLoop_Code() {
	ctx := context.Background()
	var db agent.ApprovalStore // The runner supplies its persisted approval store.
	loop := &agent.Loop{}
	token, err := agent.Check(ctx, db, 1, agent.IssueHash("title", "body"))
	if err != nil {
		return
	}
	_, _ = loop.Code(ctx, token, agent.CodeRequest{RunID: 1, Title: "title", Body: "body"})
}

func TestPublicAPIRejectsRegistryInjectionAtCompileTime(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "go", "test", "./testdata/registry_injection")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "Tools") || !strings.Contains(string(out), "cannot use registry") {
		t.Fatalf("registry injection must fail compilation: %v\n%s", err, out)
	}
}

type commandCounter struct{ calls int }

type specSink struct{}

func (*specSink) SaveSpec(_ context.Context, spec store.Spec) (store.Spec, error) {
	return spec, nil
}

func (e *commandCounter) Run(context.Context, execx.Spec) (execx.Result, error) {
	e.calls++
	return execx.Result{}, nil
}

func TestPublicAPIWithoutApprovalCannotWriteOrRunCommands(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"Run", "Investigate", "Refine", "Code"} {
		t.Run(entry, func(t *testing.T) {
			ws, err := workspace.Open(t.TempDir(), workspace.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ws.Close(true) })
			if err := ws.WriteFile("README.md", []byte("before")); err != nil {
				t.Fatal(err)
			}
			policy, err := execx.NewPolicy(ws.Dir(), map[string][]string{"test": {"/usr/bin/true"}})
			if err != nil {
				t.Fatal(err)
			}
			executor := &commandCounter{}
			env := &tools.Env{WS: ws, Exec: executor, Policy: policy, RunDir: t.TempDir()}
			p := fake.New("fake", fake.Step{ToolCalls: []llm.ToolCall{
				{ID: "write", Name: "write_file", Args: json.RawMessage(`{"path":"README.md","content":"after"}`)},
				{ID: "cmd", Name: "run_cmd", Args: json.RawMessage(`{"name":"test"}`)},
			}})
			loop := &agent.Loop{Provider: p, Model: "model", Env: env, Pricing: llm.Pricing{Free: true}}
			// Exercise the former public injection route if it ever reappears.
			// Reflection keeps this regression test compilable after its removal.
			if field := reflect.ValueOf(loop).Elem().FieldByName("Tools"); field.IsValid() && field.CanSet() {
				registry, buildErr := tools.NewCodeRegistry(env, tools.RunCmd{})
				if buildErr != nil {
					t.Fatal(buildErr)
				}
				field.Set(reflect.ValueOf(registry))
			}
			lim := agent.Limits{MaxTurns: 1}
			switch entry {
			case "Run":
				_, err = loop.Run(t.Context(), &agent.Session{Messages: []llm.Message{llm.UserText("work")}}, lim)
				if err == nil || p.Calls() != 0 {
					t.Error("bare Run reached the provider without a stage capability")
				}
			case "Investigate":
				_, _ = loop.Investigate(t.Context(), agent.InvestigateRequest{Title: "title", Limits: lim})
			case "Refine":
				_, _ = loop.Refine(t.Context(), agent.RefineRequest{RunID: 1, Store: &specSink{}, Limits: lim})
			case "Code":
				// Ignoring a failed Check cannot turn its zero value into a capability.
				token, _ := agent.Check(t.Context(), nil, 1, "issue")
				_, err = loop.Code(t.Context(), token, agent.CodeRequest{RunID: 1, Limits: lim})
				if !errors.Is(err, agent.ErrApprovalDenied) || p.Calls() != 0 {
					t.Errorf("failed Check authorized code: %v", err)
				}
			}
			data, err := ws.ReadFile("README.md")
			if err != nil || string(data) != "before" || executor.calls != 0 {
				t.Fatalf("unapproved effects: file=%q commands=%d err=%v", data, executor.calls, err)
			}
		})
	}
}
