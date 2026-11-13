package gitx

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/cgi" // #nosec G504 -- test-only git http-backend; Go 1.27 is not affected by httpoxy.
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// authRemote is a smart-HTTP git server (git http-backend over CGI) that
// refuses every request without the exact x-access-token Basic header, like a
// private GitHub repository. It records whether the token ever reached the
// request line or a non-Authorization header.
type authRemote struct {
	url    string
	mu     sync.Mutex
	denied int
	leaked bool
}

func newAuthRemote(t *testing.T, f fixture, token string) *authRemote {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	want := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	backend := &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Dir:  filepath.Dir(f.bare),
		Env: []string{
			"GIT_PROJECT_ROOT=" + filepath.Dir(f.bare), "GIT_HTTP_EXPORT_ALL=1",
			"HOME=" + f.home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=http.receivepack", "GIT_CONFIG_VALUE_0=true",
			"GIT_CONFIG_KEY_1=uploadpack.allowAnySHA1InWant", "GIT_CONFIG_VALUE_1=true",
		},
		InheritEnv: []string{"PATH"},
	}
	remote := &authRemote{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		remote.mu.Lock()
		if strings.Contains(req.URL.String(), token) {
			remote.leaked = true
		}
		for name, values := range req.Header {
			if name != "Authorization" && strings.Contains(strings.Join(values, " "), token) {
				remote.leaked = true
			}
		}
		scheme, credential, _ := strings.Cut(req.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "basic") || credential != want {
			remote.denied++
			remote.mu.Unlock()
			w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		remote.mu.Unlock()
		backend.ServeHTTP(w, req)
	}))
	t.Cleanup(server.Close)
	remote.url = server.URL + "/" + filepath.Base(f.bare)
	return remote
}

func (a *authRemote) clone(t *testing.T, token string, depth int) (*Repo, error) {
	t.Helper()
	r, err := Clone(context.Background(), CloneOptions{Dir: filepath.Join(t.TempDir(), "checkout"), Host: "github.com",
		Branch: "main", Depth: depth, Token: token, testHTTPSRemote: a.url})
	if err == nil {
		t.Cleanup(func() { _ = r.Close() })
	}
	return r, err
}

func TestPrivateRemoteRequiresHeaderOnEveryNetworkCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := gitFixture(t)
	remote := newAuthRemote(t, f, fakeToken)
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + fakeToken))

	// Without a token, or with a wrong one, the private remote refuses clone.
	for _, token := range []string{"", "ghp_wrong_credential_0000000000000000"} {
		if _, err := remote.clone(t, token, 1); err == nil {
			t.Fatalf("clone succeeded with token %q against a private remote", token)
		}
	}

	r, err := remote.clone(t, fakeToken, 1)
	if err != nil {
		t.Fatalf("authenticated clone: %v", err)
	}
	var argv []string
	r.observeCommand = func(args []string) { argv = append(argv, strings.Join(args, "\x00")) }

	// Shallow history lacks the first commit, so CheckoutSHA must re-fetch it.
	if err := r.CheckoutSHA(ctx, f.first); err != nil {
		t.Fatalf("authenticated CheckoutSHA re-fetch: %v", err)
	}
	if err := r.CreateBranch(ctx, "patchbot/issue-7"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(r.Dir(), "private.txt"), "private change\n")
	if _, err := r.CommitAll(ctx, "private change", Author{}); err != nil {
		t.Fatal(err)
	}
	if exists, err := r.RemoteBranchExists(ctx, "patchbot/issue-7"); err != nil || exists {
		t.Fatalf("branch exists before push = %v, %v", exists, err)
	}
	if err := r.Push(ctx, "patchbot/issue-7"); err != nil {
		t.Fatalf("authenticated push: %v", err)
	}
	if exists, err := r.RemoteBranchExists(ctx, "patchbot/issue-7"); err != nil || !exists {
		t.Fatalf("branch exists after push = %v, %v", exists, err)
	}
	sha, err := r.HeadSHA(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(f.git(t, f.bare, "rev-parse", "refs/heads/patchbot/issue-7")); got != sha {
		t.Fatalf("remote branch = %q, want %q", got, sha)
	}
	for _, args := range argv {
		if strings.Contains(args, fakeToken) || strings.Contains(args, basic) {
			t.Fatal("credential leaked into argv")
		}
	}
	config := read(t, filepath.Join(r.Dir(), ".git", "config"))
	if strings.Contains(config, fakeToken) || strings.Contains(config, basic) ||
		strings.Contains(strings.ToLower(config), "extraheader") || strings.Contains(config, "AUTHORIZATION") {
		t.Fatalf("credential persisted: %q", config)
	}

	// The same operations without the credential fail against the private
	// remote: drop the token from a second authenticated checkout.
	unauth, err := remote.clone(t, fakeToken, 1)
	if err != nil {
		t.Fatal(err)
	}
	unauth.token = ""
	remote.mu.Lock()
	deniedBefore := remote.denied
	remote.mu.Unlock()
	if err := unauth.CheckoutSHA(ctx, f.first); err == nil {
		t.Fatal("CheckoutSHA re-fetch succeeded without a credential")
	}
	if _, err := unauth.RemoteBranchExists(ctx, "patchbot/issue-7"); err == nil {
		t.Fatal("ls-remote succeeded without a credential")
	}
	if err := unauth.CreateBranch(ctx, "patchbot/issue-8"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(unauth.Dir(), "other.txt"), "other\n")
	if _, err := unauth.CommitAll(ctx, "other", Author{}); err != nil {
		t.Fatal(err)
	}
	err = unauth.Push(ctx, "patchbot/issue-8")
	if err == nil {
		t.Fatal("push succeeded without a credential")
	}
	if strings.Contains(err.Error(), fakeToken) || strings.Contains(err.Error(), basic) {
		t.Fatalf("unsafe error: %v", err)
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if remote.denied <= deniedBefore {
		t.Fatal("unauthenticated calls never reached the private remote")
	}
	if remote.leaked {
		t.Fatal("credential appeared in a URL or non-Authorization header")
	}
	if _, err := Clone(ctx, CloneOptions{Dir: filepath.Join(t.TempDir(), "bad"), Branch: "main", Token: fakeToken,
		testHTTPSRemote: strings.Replace(remote.url, "https://", "https://user:"+fakeToken+"@", 1)}); err == nil ||
		strings.Contains(err.Error(), fakeToken) {
		t.Fatalf("credential-bearing test URL accepted or leaked: %v", err)
	}
}

func TestPushLsRemoteFailureWithoutTokenStopsBeforePush(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := gitFixture(t)
	remote := newAuthRemote(t, f, fakeToken)
	r, err := remote.clone(t, fakeToken, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CreateBranch(ctx, "patchbot/issue-9"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(r.Dir(), "x.txt"), "x\n")
	if _, err := r.CommitAll(ctx, "x", Author{}); err != nil {
		t.Fatal(err)
	}
	r.token = ""
	var ran []string
	r.observeCommand = func(args []string) { ran = append(ran, args[0]) }
	if err := r.Push(ctx, "patchbot/issue-9"); err == nil {
		t.Fatal("push succeeded without a credential")
	}
	for _, name := range ran {
		if name == "push" {
			t.Fatal("push ran although the authenticated ls-remote guard failed")
		}
	}
}
