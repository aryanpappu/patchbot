package tools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/secret"
)

// Submission is a validated terminal result. The harness decides test status.
type Submission struct {
	Summary  string            `json:"summary,omitempty"`
	PRTitle  string            `json:"pr_title,omitempty"`
	PRBody   string            `json:"pr_body,omitempty"`
	Criteria []CriterionResult `json:"criteria,omitempty"`
	Spec     *SpecDraft        `json:"spec,omitempty"`
	Report   string            `json:"report_md,omitempty"`
}

// CriterionResult is the model's assessment of one approved acceptance criterion.
type CriterionResult struct {
	Text string `json:"text"`
	Met  bool   `json:"met"`
	Note string `json:"note,omitempty"`
}

// FileChange describes a file the spec expects to change.
type FileChange struct {
	Path   string `json:"path"`
	Change string `json:"change"`
}

// SpecDraft is the canonical spec content. Array order is significant. Nil
// arrays canonicalize to empty arrays so equivalent submissions have one hash.
type SpecDraft struct {
	Summary            string       `json:"summary"`
	AcceptanceCriteria []string     `json:"acceptance_criteria"`
	Files              []FileChange `json:"files"`
	Risks              []string     `json:"risks"`
	TestPlan           []string     `json:"test_plan"`
	OpenQuestions      []string     `json:"open_questions"`
}

// Canonical returns the deterministic JSON form bound by an approval.
func (d SpecDraft) Canonical() []byte {
	if d.AcceptanceCriteria == nil {
		d.AcceptanceCriteria = []string{}
	}
	if d.Files == nil {
		d.Files = []FileChange{}
	}
	if d.Risks == nil {
		d.Risks = []string{}
	}
	if d.TestPlan == nil {
		d.TestPlan = []string{}
	}
	if d.OpenQuestions == nil {
		d.OpenQuestions = []string{}
	}
	raw, _ := json.Marshal(d) // This struct contains only JSON-supported types.
	return raw
}

// Hash returns the SHA-256 of Canonical, as lowercase hex.
func (d SpecDraft) Hash() string { return fmt.Sprintf("%x", sha256.Sum256(d.Canonical())) }

// Markdown renders a spec without timestamps or other nondeterministic data.
func (d SpecDraft) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Specification\n\n%s\n\n## Acceptance criteria\n\n", d.Summary)
	for i, text := range d.AcceptanceCriteria {
		fmt.Fprintf(&b, "- [ ] AC%d: %s\n", i+1, text)
	}
	b.WriteString("\n## Files\n\n")
	for _, file := range d.Files {
		fmt.Fprintf(&b, "- %s: %s\n", file.Path, file.Change)
	}
	for _, section := range []struct {
		name  string
		items []string
	}{{"Risks", d.Risks}, {"Test plan", d.TestPlan}, {"Open questions", d.OpenQuestions}} {
		fmt.Fprintf(&b, "\n## %s\n\n", section.name)
		for _, item := range section.items {
			fmt.Fprintf(&b, "- %s\n", item)
		}
	}
	fmt.Fprintf(&b, "\n<!-- patchbot:spec-data %s -->\n", base64.StdEncoding.EncodeToString(d.Canonical()))
	return b.String()
}

// ParseSpec recovers the canonical fields from a deterministic rendering saved
// in store.Spec.BodyMD. This survives the workspace deletion while awaiting approval.
func ParseSpec(markdown string) (SpecDraft, error) {
	const marker = "\n<!-- patchbot:spec-data "
	_, encoded, ok := strings.Cut(markdown, marker)
	if !ok {
		return SpecDraft{}, errors.New("spec has no canonical data")
	}
	encoded, ok = strings.CutSuffix(encoded, " -->\n")
	if !ok {
		return SpecDraft{}, errors.New("spec canonical data is malformed")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return SpecDraft{}, errors.New("spec canonical data is malformed")
	}
	var draft SpecDraft
	if err := decodeArgs(raw, &draft); err != nil {
		return SpecDraft{}, err
	}
	if draft.Markdown() != markdown {
		return SpecDraft{}, errors.New("spec rendering differs from canonical data")
	}
	return draft, nil
}

// Redacted returns a deep, redacted copy, preserving structured JSON and hashes.
func (s *Submission) Redacted(r *secret.Redactor) *Submission {
	// Redact each JSON string independently, including escaped newlines in secrets.
	var value any
	raw, _ := json.Marshal(s)
	_ = json.Unmarshal(raw, &value)
	value = redactValue(value, r)
	raw, _ = json.Marshal(value)
	var out Submission
	_ = json.Unmarshal(raw, &out)
	return &out
}

func redactValue(v any, r *secret.Redactor) any {
	switch x := v.(type) {
	case string:
		return r.Redact(x)
	case []any:
		for i := range x {
			x[i] = redactValue(x[i], r)
		}
	case map[string]any:
		for k := range x {
			x[k] = redactValue(x[k], r)
		}
	}
	return v
}

// Submit is the terminal code tool. Criteria is the trusted approved checklist;
// every entry must be reported exactly once, using the approved text.
type Submit struct{ Criteria []string }

// Terminal marks submit as a stage-ending tool.
func (Submit) Terminal() bool { return true }

// Spec describes submit.
func (Submit) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "submit", Description: "Finish coding with a summary, PR title and assessment of every approved acceptance criterion. The harness verifies tests independently.", Schema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string","description":"What changed and why."},"pr_title":{"type":"string","description":"Concise pull request title."},"pr_body":{"type":"string","description":"Optional additional explanation."},"criteria":{"type":"array","description":"Each approved criterion exactly once.","items":{"type":"object","properties":{"text":{"type":"string"},"met":{"type":"boolean"},"note":{"type":"string"}},"required":["text","met"],"additionalProperties":false}}},"required":["summary","pr_title","criteria"],"additionalProperties":false}`)}
}

// Execute validates the code submission.
func (t Submit) Execute(ctx context.Context, _ *Env, raw json.RawMessage) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	var args struct {
		Summary  string            `json:"summary"`
		PRTitle  string            `json:"pr_title"`
		PRBody   string            `json:"pr_body"`
		Criteria []CriterionResult `json:"criteria"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return failed("%v", err)
	}
	if strings.TrimSpace(args.Summary) == "" || strings.TrimSpace(args.PRTitle) == "" {
		return failed("summary and pr_title must not be empty")
	}
	if len(args.Criteria) != len(t.Criteria) {
		return failed("report every approved acceptance criterion exactly once")
	}
	counts := map[string]int{}
	for _, text := range t.Criteria {
		counts[text]++
	}
	for _, entry := range args.Criteria {
		if counts[entry.Text] == 0 {
			return failed("criterion text must match the approved spec exactly once")
		}
		counts[entry.Text]--
	}
	return Result{Content: "Code submission accepted; harness verification follows.", Submission: &Submission{Summary: args.Summary, PRTitle: args.PRTitle, PRBody: args.PRBody, Criteria: args.Criteria}}, nil
}

// SubmitSpec is the terminal refine tool. Open questions yield needs_info.
type SubmitSpec struct{}

// Terminal marks submit_spec as a stage-ending tool.
func (SubmitSpec) Terminal() bool { return true }

// Spec describes submit_spec.
func (SubmitSpec) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "submit_spec", Description: "Finish refinement with a proposed spec. Include unresolved questions in open_questions; coding then waits for answers.", Schema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string","description":"Problem and intended behavior."},"acceptance_criteria":{"type":"array","description":"One to twelve concrete acceptance criteria, or empty when blocked on questions.","items":{"type":"string"}},"files":{"type":"array","description":"Likely file changes.","items":{"type":"object","properties":{"path":{"type":"string"},"change":{"type":"string"}},"required":["path","change"],"additionalProperties":false}},"risks":{"type":"array","description":"Known risks.","items":{"type":"string"}},"test_plan":{"type":"array","description":"Checks to verify the work.","items":{"type":"string"}},"open_questions":{"type":"array","description":"Unresolved questions; empty means ready for approval.","items":{"type":"string"}}},"required":["summary","acceptance_criteria","files","risks","test_plan","open_questions"],"additionalProperties":false}`)}
}

// Execute validates and renders a spec.
func (SubmitSpec) Execute(ctx context.Context, env *Env, raw json.RawMessage) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	var draft SpecDraft
	if err := decodeArgs(raw, &draft); err != nil {
		return failed("%v", err)
	}
	if strings.TrimSpace(draft.Summary) == "" {
		return failed("summary must not be empty")
	}
	if len(draft.AcceptanceCriteria) > 12 || (len(draft.AcceptanceCriteria) == 0 && len(draft.OpenQuestions) == 0) {
		return failed("provide 1 to 12 acceptance criteria or open questions")
	}
	seen := map[string]bool{}
	for _, text := range draft.AcceptanceCriteria {
		if strings.TrimSpace(text) == "" || seen[text] {
			return failed("acceptance criteria must be nonempty and distinct")
		}
		seen[text] = true
	}
	for _, list := range [][]string{draft.Risks, draft.TestPlan, draft.OpenQuestions} {
		for _, text := range list {
			if strings.TrimSpace(text) == "" {
				return failed("list entries must not be empty")
			}
		}
	}
	for _, file := range draft.Files {
		if strings.TrimSpace(file.Change) == "" {
			return failed("file change descriptions must not be empty")
		}
		if _, err := env.WS.Stat(file.Path); err != nil {
			// Stat also validates paths that do not yet exist.
			if isPolicyError(err) {
				return failure(ctx, err)
			}
		}
	}
	if len(draft.Canonical()) > 64<<10 {
		return failed("spec exceeds 64 KB")
	}
	submission := (&Submission{Summary: draft.Summary, Spec: &draft}).Redacted(env.Redactor)
	return Result{Content: submission.Spec.Markdown() + "\nSHA-256: " + submission.Spec.Hash(), Submission: submission}, nil
}

// SubmitReport is the terminal read-only investigation tool.
type SubmitReport struct{}

// Terminal marks submit_report as a stage-ending tool.
func (SubmitReport) Terminal() bool { return true }

// Spec describes submit_report.
func (SubmitReport) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "submit_report", Description: "Finish investigation with a Markdown report grounded in repository evidence.", Schema: json.RawMessage(`{"type":"object","properties":{"report_md":{"type":"string","description":"Investigation report, including evidence and remaining uncertainty."}},"required":["report_md"],"additionalProperties":false}`)}
}

// Execute validates the report.
func (SubmitReport) Execute(ctx context.Context, _ *Env, raw json.RawMessage) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	var args struct {
		Report string `json:"report_md"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return failed("%v", err)
	}
	if strings.TrimSpace(args.Report) == "" || len(args.Report) > 60000 {
		return failed("report_md must contain 1 to 60000 bytes")
	}
	return Result{Content: "Investigation report accepted.", Submission: &Submission{Report: args.Report}}, nil
}
