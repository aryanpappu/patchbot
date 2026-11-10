package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aryanpappu/patchbot/internal/execx"
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/tools"
	"github.com/aryanpappu/patchbot/internal/workspace"
)

type commandExecutor struct {
	calls  int
	check  func(execx.Spec)
	result execx.Result
	err    error
}

func (e *commandExecutor) Run(_ context.Context, spec execx.Spec) (execx.Result, error) {
	e.calls++
	if e.check != nil {
		e.check(spec)
	}
	return e.result, e.err
}

func commandEnv(t *testing.T) *tools.Env {
	t.Helper()
	runDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(runDir, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Open(repo, workspace.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close(true) })
	p, err := execx.NewPolicy(repo, map[string][]string{"test": {"/usr/bin/true"}, "extra": {"/usr/bin/true"}}, execx.PolicyOptions{AllowArgs: map[string]bool{"extra": true}})
	if err != nil {
		t.Fatal(err)
	}
	return &tools.Env{WS: ws, Policy: p, RunDir: runDir, Redactor: secret.NewRedactor("private-command-secret")}
}
func detailed(t *testing.T, r *tools.Registry, name, raw string) tools.Result {
	t.Helper()
	_, detail, err := r.DispatchDetailed(t.Context(), llm.ToolCall{ID: "c", Name: name, Args: json.RawMessage(raw)})
	if err != nil {
		t.Fatal(err)
	}
	return detail
}

func TestRunCmdIsolationAndOutput(t *testing.T) {
	t.Parallel()
	env := commandEnv(t)
	var homes []string
	e := &commandExecutor{result: execx.Result{ExitCode: 2, TimedOut: true, Output: "HEAD private-command-secret " + strings.Repeat("a", 300000) + " TAIL"}}
	e.check = func(spec execx.Spec) {
		if spec.Dir != env.WS.Dir() || spec.OutputCap != 256<<10 || spec.Timeout != execx.DefaultTimeout || spec.Redactor == nil || spec.Argv[0] != "/usr/bin/true" {
			t.Errorf("wrong execution spec: %+v", spec)
		}
		vars := map[string]string{}
		for _, entry := range spec.Env {
			key, value, _ := strings.Cut(entry, "=")
			vars[key] = value
		}
		home, tmp := vars["HOME"], vars["TMPDIR"]
		homes = append(homes, home)
		for _, dir := range []string{home, tmp} {
			rel, err := filepath.Rel(env.RunDir, dir)
			if err != nil || !filepath.IsLocal(rel) || strings.HasPrefix(dir, env.WS.Dir()+string(filepath.Separator)) {
				t.Fatal("private dir outside run dir or inside checkout")
			}
			info, err := os.Stat(dir)
			if err != nil || info.Mode().Perm() != 0o700 {
				t.Fatal("private directory not created")
			}
		}
		for key := range vars {
			if strings.Contains(key, "TOKEN") || strings.Contains(key, "SECRET") || strings.Contains(key, "API_KEY") {
				t.Fatal("secret environment variable")
			}
		}
		if vars["PATH"] != execx.TrustedPath {
			t.Fatal("ambient PATH inherited")
		}
	}
	env.Exec = e
	r, err := tools.NewCodeRegistry(env, tools.RunCmd{})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result := detailed(t, r, "run_cmd", `{"name":"test"}`)
		if !result.IsError || !result.TimedOut || result.ExitCode == nil || *result.ExitCode != 2 || len(result.Content) > 8<<10 || len(result.RecordOutput) > 256<<10 || len(result.RecordOutput) < 8<<10 {
			t.Fatalf("unexpected result (lengths %d/%d): %+v", len(result.Content), len(result.RecordOutput), result)
		}
		if !strings.Contains(result.Content, "HEAD") || !strings.Contains(result.Content, "TAIL") || strings.Contains(result.Content, "private-command-secret") {
			t.Fatal("unsafe or incomplete model output")
		}
	}
	if homes[0] == homes[1] {
		t.Fatal("command HOME was reused")
	}
	for _, home := range homes {
		if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("private environment not cleaned")
		}
	}
	for _, raw := range []string{`{"name":"missing"}`, `{"name":"test","args":["--flag"]}`, `{"name":"extra","args":["--flag"]}`, `{"name":"test","argv":["sh"]}`} {
		if result := detailed(t, r, "run_cmd", raw); !result.IsError {
			t.Fatalf("accepted %s", raw)
		}
	}
	if e.calls != 2 {
		t.Fatal("denied command reached executor")
	}
	readOnly, err := tools.NewReadOnlyRegistry(env, tools.SubmitSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if result := detailed(t, readOnly, "run_cmd", `{"name":"test"}`); !result.IsError {
		t.Fatal("readonly executes commands")
	}
}

func TestTerminalValidationAndCanonicalSpec(t *testing.T) {
	t.Parallel()
	env := commandEnv(t)
	registry, err := tools.NewReadOnlyRegistry(env, tools.SubmitSpec{}, tools.SubmitReport{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(registry.TerminalNames(), ",") != "submit_spec,submit_report" || registry.IsTerminal("read_file") {
		t.Fatal("wrong terminal names")
	}
	raw := `{"summary":"Fix greeting","acceptance_criteria":["It greets"],"files":[{"path":"new/file.go","change":"Add greeting"}],"risks":[],"test_plan":["Run test"],"open_questions":[]}`
	result := detailed(t, registry, "submit_spec", raw)
	if result.IsError || result.Submission == nil || result.Submission.Spec == nil {
		t.Fatalf("%+v", result)
	}
	draft := result.Submission.Spec
	parsed, err := tools.ParseSpec(draft.Markdown())
	if err != nil || parsed.Hash() != draft.Hash() {
		t.Fatalf("parse=%v", err)
	}
	if draft.Hash() != result.Submission.Spec.Hash() || draft.Markdown() != parsed.Markdown() {
		t.Fatal("nondeterministic rendering")
	}
	changed := strings.Replace(draft.Markdown(), "Fix greeting", "Injected text", 1)
	if _, err := tools.ParseSpec(changed); err == nil {
		t.Fatal("tampered Markdown accepted")
	}
	empty := tools.SpecDraft{}
	explicit := tools.SpecDraft{AcceptanceCriteria: []string{}, Files: []tools.FileChange{}, Risks: []string{}, TestPlan: []string{}, OpenQuestions: []string{}}
	if empty.Hash() != explicit.Hash() {
		t.Fatal("nil and empty arrays hash differently")
	}
	for _, bad := range []string{
		strings.Replace(raw, `"Fix greeting"`, `""`, 1),
		strings.Replace(raw, `["It greets"]`, `[]`, 1),
		strings.Replace(raw, `["It greets"]`, `["duplicate","duplicate"]`, 1),
		strings.Replace(raw, `new/file.go`, `../outside`, 1),
		strings.Replace(raw, `new/file.go`, `.github/workflows/test.yml`, 1),
		strings.Replace(raw, `"risks":[]`, `"risks":[""]`, 1),
	} {
		if result := detailed(t, registry, "submit_spec", bad); !result.IsError || result.Submission != nil {
			t.Fatalf("bad spec accepted %s", bad)
		}
	}
	questions := strings.Replace(strings.Replace(raw, `["It greets"]`, `[]`, 1), `"open_questions":[]`, `"open_questions":["Which greeting?"]`, 1)
	if result := detailed(t, registry, "submit_spec", questions); result.IsError || len(result.Submission.Spec.OpenQuestions) != 1 {
		t.Fatal("questions rejected")
	}
	if result := detailed(t, registry, "submit_report", `{"report_md":"Evidence"}`); result.IsError || result.Submission.Report != "Evidence" {
		t.Fatal("report rejected")
	}
	if result := detailed(t, registry, "submit_report", `{"report_md":" "}`); !result.IsError {
		t.Fatal("empty report accepted")
	}
	code, err := tools.NewCodeRegistry(env, tools.Submit{Criteria: []string{"It greets"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"summary":"x","pr_title":"y","criteria":[]}`, `{"summary":"x","pr_title":"y","criteria":[{"text":"other","met":true}]}`, `{"summary":"x","pr_title":"y","criteria":[{"text":"It greets"}]}`} {
		if result := detailed(t, code, "submit", raw); !result.IsError || result.Submission != nil {
			t.Fatalf("invalid submit accepted: %s", raw)
		}
	}
	good := `{"summary":"private-command-secret","pr_title":"Greeting","criteria":[{"text":"It greets","met":false,"note":"private-command-secret"}]}`
	safe := detailed(t, code, "submit", good)
	if safe.IsError || safe.Submission == nil || strings.Contains(safe.Submission.Summary, "private-command-secret") || strings.Contains(safe.Submission.Criteria[0].Note, "private-command-secret") {
		t.Fatal("terminal submission leaked secret")
	}
}

func TestCapOutputUTF8(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, 1, 15, 64, 8192} {
		out := tools.CapOutput(strings.Repeat("界", 10000), limit)
		if len(out) > limit || !utf8.ValidString(out) {
			t.Fatalf("bad cap %d: %d bytes", limit, len(out))
		}
	}
}
