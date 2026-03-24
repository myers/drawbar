package actions

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nektos/act/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- validatePath ---

func TestValidatePath(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty", "", false},
		{"simple", "dist/index.js", false},
		{"nested", "src/main/index.js", false},
		{"dotslash", "./index.js", false},
		{"traversal", "../index.js", true},
		{"mid_traversal", "foo/../bar", true},
		{"double_traversal", "../../etc/passwd", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePath("test", tt.value)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// --- NewActionCache ---

func TestNewActionCache(t *testing.T) {
	t.Run("explicit_dir", func(t *testing.T) {
		c := NewActionCache("/my/cache")
		assert.Equal(t, "/my/cache", c.Dir())
	})
	t.Run("empty_fallback", func(t *testing.T) {
		c := NewActionCache("")
		assert.Equal(t, os.TempDir(), c.Dir())
	})
}

// --- readActionYml ---

func TestReadActionYml(t *testing.T) {
	t.Run("action_yml", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "action.yml"), `
name: test
runs:
  using: node20
  main: index.js
`)
		action, err := readActionYml(dir)
		require.NoError(t, err)
		assert.Equal(t, "test", action.Name)
		assert.Equal(t, model.ActionRunsUsing("node20"), action.Runs.Using)
	})

	t.Run("action_yaml", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "action.yaml"), `
name: yaml-test
runs:
  using: composite
  steps: []
`)
		action, err := readActionYml(dir)
		require.NoError(t, err)
		assert.Equal(t, "yaml-test", action.Name)
	})

	t.Run("missing", func(t *testing.T) {
		dir := t.TempDir()
		_, err := readActionYml(dir)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "no action.yml or action.yaml")
	})
}

// --- buildActionEnv ---

func TestBuildActionEnv(t *testing.T) {
	meta := &ActionMeta{
		Ref: &ActionRef{Org: "actions", Repo: "cache", Ref: "v4"},
		Dir: "actions-cache-v4",
		Action: &model.Action{
			Inputs: map[string]model.Input{
				"path":    {Default: "."},
				"my-key":  {Default: "default-key"},
				"no-default": {},
			},
			Runs: model.ActionRuns{
				Env: map[string]string{"ACTION_ENV": "from-action"},
			},
		},
	}

	stepWith := map[string]string{"path": "/workspace/src"}
	stepEnv := map[string]string{"MY_VAR": "hello"}

	env := buildActionEnv(meta, stepWith, stepEnv)

	// Step env is included.
	assert.Equal(t, "hello", env["MY_VAR"])
	// Action runs.env is included.
	assert.Equal(t, "from-action", env["ACTION_ENV"])
	// Input defaults set as INPUT_ vars, hyphens preserved (matches GitHub Actions behavior).
	assert.Equal(t, "default-key", env["INPUT_MY-KEY"])
	// stepWith overrides input defaults.
	assert.Equal(t, "/workspace/src", env["INPUT_PATH"])
	// Inputs without defaults are not set.
	_, hasNoDefault := env["INPUT_NO-DEFAULT"]
	assert.False(t, hasNoDefault)
	// GITHUB_ACTION_PATH is set.
	assert.Equal(t, "/actions/actions-cache-v4", env["GITHUB_ACTION_PATH"])
}

// --- ToStepSpecs ---

func testMeta(runs model.ActionRuns) *ActionMeta {
	return &ActionMeta{
		Ref:    &ActionRef{Org: "test", Repo: "action", Ref: "v1"},
		Dir:    "test-action-v1",
		Action: &model.Action{Runs: runs},
	}
}

func TestToStepSpecs_Node_MainOnly(t *testing.T) {
	m := testMeta(model.ActionRuns{
		Using: model.ActionRunsUsingNode20,
		Main:  "dist/index.js",
	})
	specs, err := m.ToStepSpecs(nil, nil, nil)
	require.NoError(t, err)
	require.Len(t, specs, 1)
	// Node actions use direct exec (Args) to preserve hyphenated env var names.
	require.Len(t, specs[0].Args, 2)
	assert.Equal(t, "node", specs[0].Args[0])
	assert.Contains(t, specs[0].Args[1], "/actions/test-action-v1/dist/index.js")
}

func TestToStepSpecs_Node_PreMainPost(t *testing.T) {
	m := testMeta(model.ActionRuns{
		Using: model.ActionRunsUsingNode20,
		Pre:   "setup.js",
		Main:  "index.js",
		Post:  "cleanup.js",
	})
	specs, err := m.ToStepSpecs(nil, nil, nil)
	require.NoError(t, err)
	require.Len(t, specs, 3)
	assert.Contains(t, specs[0].Name, "Pre")
	assert.Contains(t, specs[0].Args[1], "setup.js")
	assert.Contains(t, specs[1].Args[1], "index.js")
	assert.Contains(t, specs[2].Name, "Post")
	assert.Contains(t, specs[2].Args[1], "cleanup.js")
}

func TestToStepSpecs_Node_NoMain(t *testing.T) {
	m := testMeta(model.ActionRuns{
		Using: model.ActionRunsUsingNode20,
		Main:  "",
	})
	_, err := m.ToStepSpecs(nil, nil, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no main entry point")
}

func TestToStepSpecs_Docker(t *testing.T) {
	m := testMeta(model.ActionRuns{
		Using: model.ActionRunsUsingDocker,
		Image: "docker://alpine:3.18",
	})
	specs, err := m.ToStepSpecs(nil, nil, nil)
	require.NoError(t, err)
	require.Len(t, specs, 1)
	assert.Equal(t, "alpine:3.18", specs[0].Image)
}

func TestToStepSpecs_Docker_Dockerfile(t *testing.T) {
	m := testMeta(model.ActionRuns{
		Using: model.ActionRunsUsingDocker,
		Image: "Dockerfile",
	})
	_, err := m.ToStepSpecs(nil, nil, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Dockerfile build")
}

func TestToStepSpecs_Docker_Entrypoint(t *testing.T) {
	m := testMeta(model.ActionRuns{
		Using:      model.ActionRunsUsingDocker,
		Image:      "docker://myimage:latest",
		Entrypoint: "/entrypoint.sh",
		Args:       []string{"--flag", "value"},
	})
	specs, err := m.ToStepSpecs(nil, nil, nil)
	require.NoError(t, err)
	require.Len(t, specs, 1)
	assert.Equal(t, []string{"/entrypoint.sh", "--flag", "value"}, specs[0].Cmd)
}

func TestToStepSpecs_Go(t *testing.T) {
	m := testMeta(model.ActionRuns{Using: model.ActionRunsUsingGo})
	specs, err := m.ToStepSpecs(nil, nil, nil)
	require.NoError(t, err)
	require.Len(t, specs, 1)
	assert.Contains(t, specs[0].Script, "go run .")
}

func TestToStepSpecs_Unsupported(t *testing.T) {
	m := testMeta(model.ActionRuns{Using: "unknown"})
	_, err := m.ToStepSpecs(nil, nil, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported action type")
}

func TestToStepSpecs_Composite(t *testing.T) {
	m := testMeta(model.ActionRuns{
		Using: model.ActionRunsUsingComposite,
		Steps: []model.Step{
			{Run: "echo hello", Name: "Greet"},
			{Run: "echo bye", Name: "Farewell"},
		},
	})
	stepEnv := map[string]string{"FOO": "bar"}
	specs, err := m.ToStepSpecs(nil, stepEnv, nil)
	require.NoError(t, err)
	require.Len(t, specs, 2)
	assert.Equal(t, "Greet", specs[0].Name)
	assert.Equal(t, "echo hello", specs[0].Script)
	assert.Equal(t, "bar", specs[0].Env["FOO"])
}

// --- actionPath ---

func TestActionPath(t *testing.T) {
	t.Run("no_subpath", func(t *testing.T) {
		m := &ActionMeta{Ref: &ActionRef{}, Dir: "actions-cache-v4"}
		assert.Equal(t, "actions-cache-v4", m.actionPath())
	})
	t.Run("with_subpath", func(t *testing.T) {
		m := &ActionMeta{Ref: &ActionRef{Path: "sub/dir"}, Dir: "my-action-v1"}
		assert.Equal(t, "my-action-v1/sub/dir", m.actionPath())
	})
}

// helpers

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}
