package config_test

import (
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/config"
)

const pinnedImage = "docker.io/library/golang:1.27@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestExecutorDefaultsAreLocal(t *testing.T) {
	t.Parallel()
	c := loadString(t, validBase, nil)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Executor.Kind != config.ExecutorLocal || c.Executor.Container() {
		t.Fatalf("default executor = %+v", c.Executor)
	}
	opts := c.CommandPolicyOptions()
	if opts.InContainer || len(opts.TrustedPath) == 0 {
		t.Fatalf("local policy options = %+v", opts)
	}
	if _, err := c.ContainerConfig(func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("ContainerConfig succeeded for the local executor")
	}
}

func TestExecutorValidation(t *testing.T) {
	t.Parallel()
	container := validBase + "[executor]\nkind = \"container\"\nimage = \"" + pinnedImage + "\"\n"
	tests := []struct {
		name    string
		file    string
		wantErr string
	}{
		{"valid podman", container, ""},
		{"valid docker", container + "runtime = \"docker\"\n", ""},
		{"keep-id podman", container + "userns = \"keep-id\"\n", ""},
		{"local ignores container fields", validBase + "[executor]\nimage = \"golang:latest\"\nnetwork = \"host\"\n", ""},
		{"unknown kind", validBase + "[executor]\nkind = \"vm\"\n", `executor.kind: "vm"`},
		{"image required", validBase + "[executor]\nkind = \"container\"\n", "executor.image: required"},
		{"image tag only", validBase + "[executor]\nkind = \"container\"\nimage = \"golang:1.27\"\n", "executor.image: image must be digest-pinned"},
		{"runtime", container + "runtime = \"lxc\"\n", `executor.runtime: "lxc"`},
		{"network", container + "network = \"bridge\"\n", `executor.network: "bridge" is not allowed`},
		{"root user", container + "user = \"0:0\"\n", "executor.user: uid and gid must be non-root"},
		{"named user", container + "user = \"root\"\n", "executor.user: want a numeric"},
		{"memory", container + "memory = \"2 GB\"\n", "executor.memory"},
		{"tmpfs", container + "tmpfs_size = \"1g,exec\"\n", "executor.tmpfs_size"},
		{"cpus", container + "cpus = \"0\"\n", "executor.cpus"},
		{"pids", container + "pids_limit = 0\n", "executor.pids_limit: must be greater than 0"},
		{"keep-id docker", container + "runtime = \"docker\"\nuserns = \"keep-id\"\n", "executor.userns"},
		{"path", container + "path = [\"bin\"]\n", "executor.path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := loadString(t, tt.file, nil).Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate error = %v\nwant it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestExecutorContainerPolicyOptions(t *testing.T) {
	t.Parallel()
	c := loadString(t, validBase+"[executor]\nkind = \"container\"\nimage = \""+pinnedImage+"\"\n", nil)
	opts := c.CommandPolicyOptions()
	if !opts.InContainer || len(opts.TrustedPath) == 0 || opts.TrustedPath[0] != "/usr/local/go/bin" {
		t.Fatalf("container policy options = %+v", opts)
	}
}

func TestExecutorShownWithSources(t *testing.T) {
	t.Parallel()
	c := loadString(t, validBase+"[executor]\nkind = \"container\"\nimage = \""+pinnedImage+"\"\n", nil)
	var b strings.Builder
	if err := c.WriteEffective(&b); err != nil {
		t.Fatal(err)
	}
	squeezed := strings.Join(strings.FieldsFunc(b.String(), func(r rune) bool { return r == ' ' }), " ")
	for _, want := range []string{"[executor]", `kind = "container" # file`, `image = "` + pinnedImage + `" # file`,
		`runtime = "podman" # default`, `network = "none" # default`, `user = "65532:65532" # default`, "pids_limit = 512 # default"} {
		if !strings.Contains(squeezed, want) {
			t.Errorf("config show lacks %q:\n%s", want, b.String())
		}
	}
}
