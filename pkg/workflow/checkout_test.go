package workflow

import (
	"testing"

	"code.forgejo.org/forgejo/runner/v12/act/model"
	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestIsCheckout(t *testing.T) {
	tests := []struct {
		uses     string
		expected bool
	}{
		{"actions/checkout@v4", true},
		{"actions/checkout@v3", true},
		{"actions/checkout@v1", true},
		{"https://code.forgejo.org/actions/checkout@v4", true},
		{"actions/setup-node@v4", false},
		{"./local-action", false},
		{"", false},
		{"actions/checkout", false}, // no version tag
	}

	for _, tt := range tests {
		t.Run(tt.uses, func(t *testing.T) {
			step := &model.Step{Uses: tt.uses}
			assert.Equal(t, tt.expected, IsCheckout(step))
		})
	}
}

func makeTaskContext(fields map[string]string) *structpb.Struct {
	m := make(map[string]*structpb.Value, len(fields))
	for k, v := range fields {
		m[k] = structpb.NewStringValue(v)
	}
	return &structpb.Struct{Fields: m}
}

func TestResolveCheckout_Defaults(t *testing.T) {
	task := &runnerv1.Task{
		Id: 1,
		Context: makeTaskContext(map[string]string{
			"repository": "TestingAdmin/test-runner",
			"sha":        "abc123",
			"ref_name":   "main",
			"token":      "test-token",
			"server_url": "http://localhost:8080/",
		}),
	}
	step := &model.Step{Uses: "actions/checkout@v4"}

	spec, err := ResolveCheckout(task, step, "")
	require.NoError(t, err)

	assert.Equal(t, "TestingAdmin/test-runner", spec.Repository)
	assert.Equal(t, "abc123", spec.SHA)
	assert.Equal(t, "main", spec.Ref)
	assert.Equal(t, "test-token", spec.Token)
	assert.Equal(t, "http://localhost:8080", spec.ServerURL)
	assert.Equal(t, 1, spec.FetchDepth)
}

func TestResolveCheckout_WithOverrides(t *testing.T) {
	task := &runnerv1.Task{
		Id: 1,
		Context: makeTaskContext(map[string]string{
			"repository": "owner/repo",
			"sha":        "abc123",
			"ref_name":   "main",
			"token":      "default-token",
			"server_url": "http://localhost:8080",
		}),
	}
	step := &model.Step{
		Uses: "actions/checkout@v4",
		With: map[string]string{
			"repository":  "other/repo",
			"ref":         "develop",
			"fetch-depth": "0",
		},
	}

	spec, err := ResolveCheckout(task, step, "")
	require.NoError(t, err)

	assert.Equal(t, "other/repo", spec.Repository)
	assert.Equal(t, "develop", spec.Ref)
	assert.Equal(t, "", spec.SHA) // explicit ref clears SHA
	assert.Equal(t, 0, spec.FetchDepth)
}

func TestResolveCheckout_CloneURLOverride(t *testing.T) {
	task := &runnerv1.Task{
		Id: 1,
		Context: makeTaskContext(map[string]string{
			"repository": "owner/repo",
			"sha":        "abc",
			"ref_name":   "main",
			"token":      "tok",
			"server_url": "http://localhost:8080",
		}),
	}
	step := &model.Step{Uses: "actions/checkout@v4"}

	spec, err := ResolveCheckout(task, step, "http://forgejo.gitea.svc:80")
	require.NoError(t, err)

	assert.Equal(t, "http://forgejo.gitea.svc:80", spec.ServerURL)
}

func TestCheckoutSpec_ToStepSpecs_WithToken(t *testing.T) {
	spec := &CheckoutSpec{
		Repository: "owner/repo",
		Ref:        "main",
		SHA:        "abc123",
		Token:      "mytoken",
		ServerURL:  "http://gitea.svc",
		FetchDepth: 1,
	}

	steps := spec.ToStepSpecs()
	require.Len(t, steps, 3) // clone, fetch, checkout

	// Clone step.
	assert.Equal(t, "Checkout: clone", steps[0].Name)
	assert.Contains(t, steps[0].Args, "--depth=1")
	assert.Contains(t, steps[0].Args, "--branch=main")
	assert.Contains(t, steps[0].Args, "http://gitea.svc/owner/repo.git")
	// Token NOT in clone URL.
	for _, arg := range steps[0].Args {
		assert.NotContains(t, arg, "mytoken")
	}
	assert.Equal(t, "/shim/askpass.sh", steps[0].Env["GIT_ASKPASS"])
	assert.Equal(t, "mytoken", steps[0].Env["GIT_AUTH_TOKEN"])

	// Fetch specific SHA.
	assert.Equal(t, "Checkout: fetch", steps[1].Name)
	assert.Equal(t, []string{"git", "-C", "/workspace", "fetch", "--depth=1", "origin", "--", "abc123"}, steps[1].Args)

	// Checkout FETCH_HEAD.
	assert.Equal(t, "Checkout: checkout", steps[2].Name)
	assert.Equal(t, []string{"git", "-C", "/workspace", "checkout", "FETCH_HEAD"}, steps[2].Args)
}

func TestCheckoutSpec_FullClone(t *testing.T) {
	spec := &CheckoutSpec{
		Repository: "owner/repo",
		Ref:        "main",
		SHA:        "abc123",
		Token:      "tok",
		ServerURL:  "http://gitea.svc",
		FetchDepth: 0, // full clone
	}

	steps := spec.ToStepSpecs()
	require.Len(t, steps, 2) // clone + checkout (no fetch for full clone)

	for _, arg := range steps[0].Args {
		assert.NotContains(t, arg, "--depth")
	}
	assert.Equal(t, "Checkout: checkout", steps[1].Name)
	assert.Equal(t, []string{"git", "-C", "/workspace", "checkout", "--", "abc123"}, steps[1].Args)
}

func TestCheckoutSpec_NoToken(t *testing.T) {
	spec := &CheckoutSpec{
		Repository: "owner/repo",
		Ref:        "main",
		ServerURL:  "http://gitea.svc",
		FetchDepth: 1,
	}

	steps := spec.ToStepSpecs()
	require.Len(t, steps, 1) // clone only (no SHA)

	assert.Contains(t, steps[0].Args, "http://gitea.svc/owner/repo.git")
	assert.Nil(t, steps[0].Env)
}

func TestCheckoutSpec_NoTokenInURL(t *testing.T) {
	spec := &CheckoutSpec{
		Repository: "owner/repo",
		Ref:        "main",
		Token:      "secret-token",
		ServerURL:  "http://gitea.svc",
		FetchDepth: 1,
	}

	url := spec.buildCloneURL()
	assert.NotContains(t, url, "secret-token")
	assert.NotContains(t, url, "x-access-token")
	assert.Equal(t, "http://gitea.svc/owner/repo.git", url)
}
