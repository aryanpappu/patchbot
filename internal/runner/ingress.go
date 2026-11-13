package runner

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/store"
)

// ReconcileIngress reads current GitHub state; webhook content never authorizes
// an action. Comment reconciliation uses the same gate as polling.
func (r *Runner) ReconcileIngress(ctx context.Context, repo string, number int64, labels bool) error {
	if r.cfg.Repos[repo] == nil || number <= 0 {
		return nil
	}
	meta, err := r.gh.GetRepo(ctx, ref(repo))
	if err != nil {
		return err
	}
	if !strings.EqualFold(meta.FullName, repo) {
		return nil
	}
	issue, err := r.gh.GetIssue(ctx, ref(repo), int(number))
	if err != nil {
		return err
	}
	if issue.Number != int(number) || issue.IsPullRequest || issue.State != "open" {
		return nil
	}
	if issue.HTMLURL != "" && !strings.EqualFold(issue.HTMLURL, "https://github.com/"+repo+"/issues/"+strconv.FormatInt(number, 10)) {
		return nil
	}
	page, err := r.db.ListRuns(ctx, store.RunFilter{Repo: repo, IssueNumber: number, Limit: 500})
	if err != nil {
		return err
	}
	if !labels {
		for _, run := range page.Runs {
			if waiting(run.Status) && !run.DryRun {
				if r.expired(run) {
					return r.expire(ctx, run)
				}
				if err := r.syncRun(ctx, run); err != nil && !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrActiveRunExists) {
					return err
				}
			}
		}
		return nil
	}

	names := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		if _, ok := r.cfg.Webhook.TriggerLabels[label.Name]; ok {
			names = append(names, label.Name)
		}
	}
	sort.Strings(names)
	label := ""
	if len(names) > 0 {
		label = names[0]
	}
	generation, err := r.db.ObserveTriggerLabel(ctx, repo, number, label)
	if err != nil {
		return err
	}
	if label == "" {
		return nil
	}
	key := "label:" + repo + ":" + strconv.FormatInt(number, 10) + ":" + strconv.FormatInt(generation, 10)
	_, err = r.Start(ctx, Request{Repo: repo, Issue: number, Mode: r.cfg.Webhook.TriggerLabels[label], IngressKey: key})
	if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrActiveRunExists) {
		return nil
	}
	return err
}

// SyncLabels recovers missed issue label deliveries. The optional listing seam
// keeps clients that only support the original runner API usable.
func (r *Runner) SyncLabels(ctx context.Context) error {
	if !r.cfg.Webhook.Enabled {
		return nil
	}
	listing, ok := r.gh.(interface {
		ListOpenIssues(context.Context, github.RepoRef, int) ([]github.Issue, error)
	})
	if !ok {
		return nil
	}
	var errs []error
	for repo := range r.cfg.Repos {
		for page := 1; ; page++ {
			issues, err := listing.ListOpenIssues(ctx, ref(repo), page)
			if err != nil {
				errs = append(errs, err)
				break
			}
			for _, issue := range issues {
				if !issue.IsPullRequest {
					if err := r.ReconcileIngress(ctx, repo, int64(issue.Number), true); err != nil {
						errs = append(errs, err)
					}
				}
			}
			if len(issues) < 100 {
				break
			}
		}
	}
	return errors.Join(errs...)
}
