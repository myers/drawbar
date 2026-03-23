package workflow

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/nektos/act/pkg/model"
	runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
	"github.com/myers/drawbar/pkg/types"
)

// IsCheckout returns true if the step uses actions/checkout (any version).
func IsCheckout(step *model.Step) bool {
	uses := step.Uses
	if uses == "" {
		return false
	}
	// Match: actions/checkout@v1, actions/checkout@v4, etc.
	// Also match Forgejo mirrors like {server}/actions/checkout@v4
	parts := strings.Split(uses, "@")
	if len(parts) != 2 {
		return false
	}
	ref := parts[0]
	return ref == "actions/checkout" || strings.HasSuffix(ref, "/actions/checkout")
}

// CheckoutSpec describes a built-in checkout operation.
type CheckoutSpec struct {
	Repository string // "owner/repo"
	Ref        string // branch/tag name (not refs/heads/... — just the name)
	SHA        string // commit SHA
	Token      string // auth token
	ServerURL  string // base URL for cloning
	FetchDepth int    // 0 = full clone, >0 = shallow
}

// ResolveCheckout extracts checkout parameters from the task context and step inputs.
func ResolveCheckout(task *runnerv1.Task, step *model.Step, cloneURLOverride string) (*CheckoutSpec, error) {
	ctx := task.GetContext()
	if ctx == nil {
		return nil, fmt.Errorf("task has no context")
	}
	fields := ctx.GetFields()

	spec := &CheckoutSpec{
		Repository: fields["repository"].GetStringValue(),
		SHA:        fields["sha"].GetStringValue(),
		Ref:        fields["ref_name"].GetStringValue(),
		Token:      fields["token"].GetStringValue(),
		ServerURL:  strings.TrimRight(fields["server_url"].GetStringValue(), "/"),
		FetchDepth: 1, // shallow clone by default
	}

	// Override with step inputs if provided.
	if v, ok := step.With["repository"]; ok && v != "" {
		spec.Repository = v
	}
	if v, ok := step.With["ref"]; ok && v != "" {
		spec.Ref = v
		spec.SHA = "" // explicit ref overrides SHA
	}
	if v, ok := step.With["token"]; ok && v != "" {
		spec.Token = v
	}
	if v, ok := step.With["fetch-depth"]; ok && v != "" {
		if depth, err := strconv.Atoi(v); err == nil {
			spec.FetchDepth = depth
		}
	}

	// Apply clone URL override.
	if cloneURLOverride != "" {
		spec.ServerURL = strings.TrimRight(cloneURLOverride, "/")
	}

	// Validate required fields.
	if spec.Repository == "" {
		return nil, fmt.Errorf("checkout: repository is empty")
	}
	if spec.ServerURL == "" {
		return nil, fmt.Errorf("checkout: server_url is empty")
	}

	return spec, nil
}

// ToStepSpecs converts a CheckoutSpec into one or more StepSpecs using
// structured args (no shell interpolation) to prevent injection attacks.
func (c *CheckoutSpec) ToStepSpecs() []types.StepSpec {
	var specs []types.StepSpec

	// Build env vars for git auth (GIT_ASKPASS avoids embedding token in URL).
	var env map[string]string
	if c.Token != "" {
		env = map[string]string{
			"GIT_ASKPASS":        "/shim/askpass.sh",
			"GIT_AUTH_TOKEN":     c.Token,
			"GIT_TERMINAL_PROMPT": "0",
		}
	}

	cloneURL := c.buildCloneURL()

	// Step 1: git clone. Workspace is always emptyDir (ZFS snapshot cache
	// only bind-mounts specific paths like target/, not the whole workspace).
	cloneArgs := []string{"git", "clone"}
	if c.FetchDepth > 0 {
		cloneArgs = append(cloneArgs, fmt.Sprintf("--depth=%d", c.FetchDepth))
	}
	if c.Ref != "" {
		cloneArgs = append(cloneArgs, fmt.Sprintf("--branch=%s", c.Ref))
	}
	cloneArgs = append(cloneArgs, "--", cloneURL, "/workspace")
	specs = append(specs, types.StepSpec{
		Name: "Checkout: clone",
		Args: cloneArgs,
		Env:  env,
	})

	// Step 2: fetch specific SHA and checkout.
	// For shallow clones, the SHA may not be the branch tip. Fetch it into
	// FETCH_HEAD and check that out.
	if c.SHA != "" && c.FetchDepth > 0 {
		specs = append(specs, types.StepSpec{
			Name: "Checkout: fetch",
			Args: []string{"git", "-C", "/workspace", "fetch", "--depth=1", "origin", "--", c.SHA},
			Env:  env,
		})
		specs = append(specs, types.StepSpec{
			Name: "Checkout: checkout",
			Args: []string{"git", "-C", "/workspace", "checkout", "FETCH_HEAD"},
			Env:  env,
		})
	} else if c.SHA != "" {
		// Full clone: SHA is already in the object store.
		specs = append(specs, types.StepSpec{
			Name: "Checkout: checkout",
			Args: []string{"git", "-C", "/workspace", "checkout", "--", c.SHA},
			Env:  env,
		})
	}

	return specs
}

func (c *CheckoutSpec) buildCloneURL() string {
	// Never embed the token in the URL — use GIT_ASKPASS instead.
	return fmt.Sprintf("%s/%s.git", c.ServerURL, c.Repository)
}

