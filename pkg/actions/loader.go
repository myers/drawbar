package actions

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nektos/act/pkg/model"
	"github.com/myers/drawbar/pkg/types"
)

// ActionMeta contains the parsed action.yml plus resolved execution info.
type ActionMeta struct {
	Ref    *ActionRef
	Action *model.Action
	Dir    string // directory name in /actions volume
}

// ActionCache is a filesystem cache of cloned action repos.
// Once an action is cloned, it's reused across all builds.
type ActionCache struct {
	dir string
}

// NewActionCache creates an ActionCache rooted at dir.
// If dir is empty, os.TempDir() is used as a fallback.
func NewActionCache(dir string) *ActionCache {
	if dir == "" {
		dir = os.TempDir()
	}
	return &ActionCache{dir: dir}
}

// Dir returns the cache root directory.
func (c *ActionCache) Dir() string {
	return c.dir
}

// LoadAction loads an action's metadata. Clones the repo only on first use;
// subsequent calls use the cached clone.
func (c *ActionCache) LoadAction(ref *ActionRef, defaultActionsURL, token string) (*ActionMeta, error) {
	cloneURL := ref.CloneURL(defaultActionsURL)
	dir := ref.ActionDir()
	cacheDir := filepath.Join(c.dir, "actions-repo-cache")

	// Ensure cache directory exists.
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating action cache dir: %w", err)
	}

	cachedPath := filepath.Join(cacheDir, dir)

	// Check if already cached.
	if _, err := os.Stat(filepath.Join(cachedPath, ".git")); err == nil {
		slog.Info("using cached action", "action", ref.String(), "path", cachedPath)
	} else {
		// Clone fresh.
		slog.Info("cloning action (first use)", "action", ref.String(), "clone_url", cloneURL)

		// Remove any partial clone.
		os.RemoveAll(cachedPath)

		cmd := exec.Command("git", "clone", "--depth=1", "--branch="+ref.Ref,
			"--", cloneURL, cachedPath)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		output, err := cmd.CombinedOutput()
		if err != nil {
			os.RemoveAll(cachedPath) // clean up failed clone
			return nil, fmt.Errorf("cloning action %s: %w\n%s", ref.String(), err, string(output))
		}
		slog.Info("action cached", "action", ref.String())
	}

	// Determine the action.yml path (may be in a subdirectory).
	actionDir := cachedPath
	if ref.Path != "" {
		actionDir = filepath.Join(cachedPath, ref.Path)
	}

	// Read action.yml or action.yaml.
	action, err := readActionYml(actionDir)
	if err != nil {
		return nil, fmt.Errorf("reading action.yml for %s: %w", ref.String(), err)
	}

	// Validate that action entry points don't escape the action directory.
	for name, value := range map[string]string{
		"runs.main":       action.Runs.Main,
		"runs.pre":        action.Runs.Pre,
		"runs.post":       action.Runs.Post,
		"runs.entrypoint": action.Runs.Entrypoint,
	} {
		if err := validatePath(name, value); err != nil {
			return nil, fmt.Errorf("action %s: %w", ref.String(), err)
		}
	}

	return &ActionMeta{
		Ref:    ref,
		Action: action,
		Dir:    dir,
	}, nil
}

// validatePath checks that a path doesn't escape its parent directory via traversal.
func validatePath(name, value string) error {
	if value == "" {
		return nil
	}
	for _, part := range strings.Split(filepath.ToSlash(value), "/") {
		if part == ".." {
			return fmt.Errorf("%s contains path traversal: %q", name, value)
		}
	}
	cleaned := filepath.Clean(value)
	if strings.HasPrefix(cleaned, "..") {
		return fmt.Errorf("%s escapes action directory: %q", name, value)
	}
	return nil
}

func readActionYml(dir string) (*model.Action, error) {
	for _, name := range []string{"action.yml", "action.yaml"} {
		action, err := tryReadActionFile(filepath.Join(dir, name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		return action, nil
	}
	return nil, fmt.Errorf("no action.yml or action.yaml found in %s", dir)
}

func tryReadActionFile(path string) (*model.Action, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return model.ReadAction(f)
}
// ExpandCtx carries state through recursive composite action expansion.
type ExpandCtx struct {
	Cache      *ActionCache
	ActionsURL string
	Token      string
	visited    map[string]bool // cycle detection
	Nested     []*ActionMeta   // actions discovered during expansion (for cache PVC mounts)
}

// NewExpandCtx creates an expansion context for recursive action loading.
func NewExpandCtx(cache *ActionCache, actionsURL, token string) *ExpandCtx {
	return &ExpandCtx{
		Cache:      cache,
		ActionsURL: actionsURL,
		Token:      token,
		visited:    make(map[string]bool),
	}
}

// ToStepSpecs converts an action into one or more StepSpecs.
// If ectx is non-nil, nested uses: inside composite actions are recursively expanded.
func (m *ActionMeta) ToStepSpecs(stepWith map[string]string, stepEnv map[string]string, ectx *ExpandCtx) ([]types.StepSpec, error) {
	action := m.Action
	env := buildActionEnv(m, stepWith, stepEnv)

	switch action.Runs.Using {
	case model.ActionRunsUsingNode12, model.ActionRunsUsingNode16,
		model.ActionRunsUsingNode20, model.ActionRunsUsingNode24:
		return m.nodeStepSpecs(env)

	case model.ActionRunsUsingDocker:
		return m.dockerStepSpecs(env)

	case model.ActionRunsUsingComposite:
		return m.compositeStepSpecs(stepWith, stepEnv, ectx)

	case model.ActionRunsUsingGo:
		return []types.StepSpec{{
			Name:   m.Ref.String(),
			Script: fmt.Sprintf("cd /actions/%s && go run .", m.Dir),
			Env:    env,
		}}, nil

	default:
		return nil, fmt.Errorf("unsupported action type: %s", action.Runs.Using)
	}
}

func (m *ActionMeta) nodeStepSpecs(env map[string]string) ([]types.StepSpec, error) {
	var specs []types.StepSpec
	action := m.Action
	actionPath := m.actionPath()

	if action.Runs.Pre != "" {
		specs = append(specs, types.StepSpec{
			Name:   fmt.Sprintf("Pre %s", m.Ref.String()),
			Script: fmt.Sprintf("node /actions/%s/%s", actionPath, action.Runs.Pre),
			Env:    env,
		})
	}

	if action.Runs.Main == "" {
		return nil, fmt.Errorf("action %s has no main entry point", m.Ref.String())
	}
	specs = append(specs, types.StepSpec{
		Name:   m.Ref.String(),
		Script: fmt.Sprintf("node /actions/%s/%s", actionPath, action.Runs.Main),
		Env:    env,
	})

	if action.Runs.Post != "" {
		specs = append(specs, types.StepSpec{
			Name:   fmt.Sprintf("Post %s", m.Ref.String()),
			Script: fmt.Sprintf("node /actions/%s/%s", actionPath, action.Runs.Post),
			Env:    env,
		})
	}

	return specs, nil
}

func (m *ActionMeta) dockerStepSpecs(env map[string]string) ([]types.StepSpec, error) {
	action := m.Action
	image := action.Runs.Image

	if strings.HasPrefix(image, "docker://") {
		image = strings.TrimPrefix(image, "docker://")
	} else if image == "Dockerfile" {
		return nil, fmt.Errorf("Docker action %s uses Dockerfile build (not yet supported)", m.Ref.String())
	}

	spec := types.StepSpec{
		Name:  m.Ref.String(),
		Image: image,
		Env:   env,
	}

	if action.Runs.Entrypoint != "" {
		spec.Cmd = []string{action.Runs.Entrypoint}
		spec.Cmd = append(spec.Cmd, action.Runs.Args...)
	}

	return []types.StepSpec{spec}, nil
}

func (m *ActionMeta) compositeStepSpecs(stepWith map[string]string, stepEnv map[string]string, ectx *ExpandCtx) ([]types.StepSpec, error) {
	var specs []types.StepSpec
	for _, step := range m.Action.Runs.Steps {
		if step.Run != "" {
			env := make(map[string]string)
			for k, v := range stepEnv {
				env[k] = v
			}
			for k, v := range step.GetEnv() {
				env[k] = v
			}
			specs = append(specs, types.StepSpec{
				Name:   step.Name,
				Shell:  step.Shell,
				Script: step.Run,
				Env:    env,
			})
		} else if step.Uses != "" {
			if ectx == nil || ectx.Cache == nil {
				slog.Warn("nested action in composite skipped (no expansion context)",
					"parent", m.Ref.String(), "nested", step.Uses)
				continue
			}

			ref, err := ParseActionRef(step.Uses)
			if err != nil {
				slog.Warn("unsupported nested action reference",
					"parent", m.Ref.String(), "uses", step.Uses, "error", err)
				continue
			}

			refKey := ref.String()
			if ectx.visited[refKey] {
				return nil, fmt.Errorf("circular composite action: %s → %s", m.Ref.String(), refKey)
			}
			ectx.visited[refKey] = true

			nested, err := ectx.Cache.LoadAction(ref, ectx.ActionsURL, ectx.Token)
			if err != nil {
				return nil, fmt.Errorf("loading nested action %s in %s: %w", refKey, m.Ref.String(), err)
			}

			nestedSpecs, err := nested.ToStepSpecs(step.With, step.GetEnv(), ectx)
			delete(ectx.visited, refKey)
			if err != nil {
				return nil, fmt.Errorf("expanding nested action %s: %w", refKey, err)
			}

			ectx.Nested = append(ectx.Nested, nested)
			specs = append(specs, nestedSpecs...)
		}
	}
	return specs, nil
}

func (m *ActionMeta) actionPath() string {
	if m.Ref.Path != "" {
		return m.Dir + "/" + m.Ref.Path
	}
	return m.Dir
}

func buildActionEnv(meta *ActionMeta, stepWith map[string]string, stepEnv map[string]string) map[string]string {
	env := make(map[string]string)

	for k, v := range stepEnv {
		env[k] = v
	}
	for k, v := range meta.Action.Runs.Env {
		env[k] = v
	}
	for name, input := range meta.Action.Inputs {
		if input.Default != "" {
			envKey := "INPUT_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
			env[envKey] = input.Default
		}
	}
	for k, v := range stepWith {
		envKey := "INPUT_" + strings.ToUpper(strings.ReplaceAll(k, "-", "_"))
		env[envKey] = v
	}
	env["GITHUB_ACTION_PATH"] = "/actions/" + meta.actionPath()

	return env
}

// ReadAction is exported for testing.
func ReadAction(r io.Reader) (*model.Action, error) {
	return model.ReadAction(r)
}
