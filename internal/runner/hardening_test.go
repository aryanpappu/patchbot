package runner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/runner"
)

type credentialEchoClient struct{ github.Client }

func (c credentialEchoClient) CreatePR(ctx context.Context, ref github.RepoRef, input github.NewPR) (github.PullRequest, error) {
	pr, err := c.Client.CreatePR(ctx, ref, input)
	pr.HTMLURL += "?echo=fixture-credential"
	return pr, err
}

func TestUpstreamPRURLIsRedactedBeforeStorage(t *testing.T) {
	f := newFixture(t, "true", specStep(), writeStep(), submitStep())
	f.opts.GitHub = credentialEchoClient{Client: f.opts.GitHub}
	var err error
	f.r, err = runner.New(t.Context(), f.cfg, f.db, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	run := f.start("fix", false)
	f.approve(run.ID)
	if err := f.r.Execute(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	got := f.get(run.ID)
	if strings.Contains(got.PRURL, "fixture-credential") || !strings.Contains(got.PRURL, "[redacted]") {
		t.Fatal("PR URL credential persisted")
	}
	events, err := f.db.EventsAfter(t.Context(), run.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if strings.Contains(string(e.PayloadJSON), "fixture-credential") {
			t.Fatal("event credential persisted")
		}
	}
}
