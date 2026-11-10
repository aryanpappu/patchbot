package gitx

import (
	"context"
	"errors"
	"strings"
)

// CommitAll stages only enumerated Status paths, then commits with the controlled
// author/committer identity and no hooks or signing. Policy checks for protected
// application files belong to the caller's workspace/orchestration layer.
func (r *Repo) CommitAll(ctx context.Context, msg string, author Author) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.verify(); err != nil {
		return "", err
	}
	if msg == "" || strings.ContainsRune(msg, 0) {
		return "", errors.New("invalid commit message")
	}
	if author == (Author{}) {
		author = Author{"patchbot", "patchbot@users.noreply.github.com"}
	}
	if author.Name == "" || author.Email == "" || strings.ContainsAny(author.Name+author.Email, "\r\n\x00<>") {
		return "", errors.New("invalid commit identity")
	}
	r.author = author
	changes, err := r.status(ctx)
	if err != nil {
		return "", err
	}
	paths, err := changedPaths(changes)
	if err != nil {
		return "", err
	}
	if err := r.noSubmodules(ctx); err != nil {
		return "", err
	}
	if err := r.stage(ctx, "", paths); err != nil {
		return "", err
	}
	if _, err := r.complete(ctx, false, "commit", "--no-verify", "--no-gpg-sign", "-m", msg, "--"); err != nil {
		return "", err
	}
	return r.head(ctx)
}

// Push verifies metadata and branch guards before pushing an explicit refspec to
// the original, constructed URL. It never consults remote.pushurl or push defaults.
// The live remote HEAD check is intentionally unauthenticated: only clone and
// push receive the token. An inaccessible remote fails closed.
func (r *Repo) Push(ctx context.Context, branch string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !validBranch(branch) || !strings.HasPrefix(branch, "patchbot/") || branch == r.defaultBranch {
		return ErrBranchGuard
	}
	if err := r.verify(); err != nil {
		return err
	}
	if err := r.noSubmodules(ctx); err != nil {
		return err
	}
	changes, err := r.status(ctx)
	if err != nil {
		return err
	}
	if _, err := changedPaths(changes); err != nil {
		return err
	}
	head, err := r.complete(ctx, false, "ls-remote", "--symref", "--", r.remote, "HEAD")
	if err != nil {
		return err
	}
	var liveDefault string
	for _, line := range strings.Split(head, "\n") {
		if ref, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			if name, suffix, ok := strings.Cut(ref, "\t"); ok && suffix == "HEAD" {
				liveDefault = name
			}
		}
	}
	if liveDefault == "" || liveDefault == branch {
		return ErrBranchGuard
	}
	_, err = r.complete(ctx, true, "push", "--no-verify", "--recurse-submodules=no", "--", r.remote, "HEAD:refs/heads/"+branch)
	return err
}

// RemoteBranchExists queries exactly one branch without inheriting credentials.
func (r *Repo) RemoteBranchExists(ctx context.Context, branch string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !validBranch(branch) {
		return false, ErrBranchGuard
	}
	if err := r.verify(); err != nil {
		return false, err
	}
	out, err := r.complete(ctx, false, "ls-remote", "--heads", "--", r.remote, "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		sha, ref, ok := strings.Cut(line, "\t")
		if ok && ref == "refs/heads/"+branch && objectID.MatchString(sha) {
			return true, nil
		}
	}
	return false, nil
}
