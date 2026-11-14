package runner

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/store"
)

// ReconcileIngress reads current GitHub state; webhook content never authorizes
// an action. Comment reconciliation uses the same gate as polling.
func (r *Runner) ReconcileIngress(ctx context.Context, repo string, number int64, labels bool) error {
	return r.ReconcileIssue(ctx, repo, number, !labels, labels)
}

// ReconcileIssue reconciles one issue once for a coalesced group of hints. It
// fetches the repository and issue once and then applies the comment and/or
// label reconciliation. A repository or issue that is gone (404/410) has
// nothing left to reconcile and is reported as done.
func (r *Runner) ReconcileIssue(ctx context.Context, repo string, number int64, comments, labels bool) error {
	err := r.reconcileIssue(ctx, repo, number, comments, labels)
	if Gone(err) {
		return nil
	}
	return err
}

// Gone reports whether a GitHub error means the resource no longer exists
// (deleted or transferred), so retrying cannot succeed.
func Gone(err error) bool {
	var api *github.APIError
	return errors.As(err, &api) && (api.StatusCode == http.StatusNotFound || api.StatusCode == http.StatusGone)
}

func (r *Runner) reconcileIssue(ctx context.Context, repo string, number int64, comments, labels bool) error {
	if !r.repos.Has(repo) || number <= 0 {
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
	if !r.ingressIssue(repo, issue) || issue.Number != int(number) {
		return nil
	}
	if comments {
		if err := r.reconcileComments(ctx, repo, number); err != nil {
			return err
		}
	}
	if labels {
		return r.applyTriggerLabel(ctx, repo, number, r.triggerLabel(issue.Labels))
	}
	return nil
}

func (r *Runner) ingressIssue(repo string, issue github.Issue) bool {
	if issue.Number <= 0 || issue.IsPullRequest || issue.State != "open" {
		return false
	}
	return issue.HTMLURL == "" || strings.EqualFold(issue.HTMLURL, "https://github.com/"+repo+"/issues/"+strconv.Itoa(issue.Number))
}

func (r *Runner) reconcileComments(ctx context.Context, repo string, number int64) error {
	page, err := r.db.ListRuns(ctx, store.RunFilter{Repo: repo, IssueNumber: number, Limit: 500})
	if err != nil {
		return err
	}
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

// triggerLabel selects the lexically first configured trigger label, or "".
func (r *Runner) triggerLabel(labels []github.Label) string {
	names := make([]string, 0, len(labels))
	for _, label := range labels {
		if _, ok := r.cfg.Webhook.TriggerLabels[label.Name]; ok {
			names = append(names, label.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return names[0]
}

// applyTriggerLabel latches label and starts at most one run per generation.
// A generation is marked handled once its run exists, so later polls skip it.
func (r *Runner) applyTriggerLabel(ctx context.Context, repo string, number int64, label string) error {
	state, err := r.db.ObserveTriggerLabel(ctx, repo, number, label)
	if err != nil || label == "" || state.Handled {
		return err
	}
	key := "label:" + repo + ":" + strconv.FormatInt(number, 10) + ":" + strconv.FormatInt(state.Generation, 10)
	_, err = r.Start(ctx, Request{Repo: repo, Issue: number, Mode: r.cfg.Webhook.TriggerLabels[label], IngressKey: key})
	switch {
	case errors.Is(err, store.ErrActiveRunExists):
		// Another run owns the issue; a later poll retries this generation.
		return nil
	case err == nil || errors.Is(err, store.ErrConflict):
		return r.db.MarkTriggerHandled(ctx, repo, number, state.Generation)
	default:
		return err
	}
}

// baselineTriggerLabel latches a label seen on a repository's first sweep
// without starting work, so enabling webhooks does not mass-start runs.
func (r *Runner) baselineTriggerLabel(ctx context.Context, repo string, number int64, label string) error {
	state, err := r.db.ObserveTriggerLabel(ctx, repo, number, label)
	if err != nil || label == "" || state.Handled {
		return err
	}
	return r.db.MarkTriggerHandled(ctx, repo, number, state.Generation)
}

// SyncLabels recovers missed issue label deliveries. It uses the labels in the
// open-issue listing, fetches each repository once per sweep, and touches only
// issues whose trigger label is new, unhandled or removed. The first completed
// sweep of a repository is a baseline that starts nothing. The optional
// listing seam keeps clients that only support the original runner API usable.
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
	repos := r.repos.Names()
	var errs []error
	for _, repo := range repos {
		if err := r.syncRepoLabels(ctx, listing, repo); err != nil {
			errs = append(errs, err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(errs...)
}

func (r *Runner) syncRepoLabels(ctx context.Context, listing interface {
	ListOpenIssues(context.Context, github.RepoRef, int) ([]github.Issue, error)
}, repo string) error {
	meta, err := r.gh.GetRepo(ctx, ref(repo))
	if err != nil {
		return err
	}
	if !strings.EqualFold(meta.FullName, repo) {
		return nil
	}
	baselined, err := r.db.RepoBaselined(ctx, repo)
	if err != nil {
		return err
	}
	known, err := r.db.TriggerLabels(ctx, repo)
	if err != nil {
		return err
	}
	var errs []error
	for page := 1; ; page++ {
		issues, err := listing.ListOpenIssues(ctx, ref(repo), page)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		for _, issue := range issues {
			if !r.ingressIssue(repo, issue) {
				continue
			}
			number := int64(issue.Number)
			label := r.triggerLabel(issue.Labels)
			state, seen := known[number]
			switch {
			case label == "" && (!seen || state.Label == ""):
				continue // nothing latched and nothing to rearm
			case label != "" && seen && state.Label == label && state.Handled:
				continue // already started or baselined
			case label == "":
				err = r.applyTriggerLabel(ctx, repo, number, "") // observed removal rearms
			case !baselined:
				err = r.baselineTriggerLabel(ctx, repo, number, label)
			default:
				err = r.applyTriggerLabel(ctx, repo, number, label)
			}
			if err != nil {
				errs = append(errs, err)
			}
		}
		if len(issues) < 100 {
			break
		}
	}
	if !baselined && len(errs) == 0 {
		if err := r.db.MarkRepoBaselined(ctx, repo); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
