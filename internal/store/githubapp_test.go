package store

import (
	"errors"
	"testing"
)

func TestGitHubAppIsSingleRow(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := t.Context()
	if _, found, err := s.GitHubApp(ctx); err != nil || found {
		t.Fatalf("empty: %v %v", found, err)
	}
	for _, bad := range []GitHubApp{{}, {AppID: 1}, {Slug: "x"}, {AppID: 1, Slug: "x", InstallationID: -1}} {
		if err := s.SaveGitHubApp(ctx, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if err := s.SaveGitHubApp(ctx, GitHubApp{AppID: 42, Slug: "patchbot-abc"}); err != nil {
		t.Fatal(err)
	}
	app, found, err := s.GitHubApp(ctx)
	if err != nil || !found || app.AppID != 42 || app.Slug != "patchbot-abc" || app.InstallationID != 0 || app.CreatedAt.IsZero() {
		t.Fatalf("%+v %v %v", app, found, err)
	}
	if err := s.SaveGitHubApp(ctx, GitHubApp{AppID: 43, Slug: "other"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second save: %v", err)
	}
}
