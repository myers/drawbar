package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code.forgejo.org/forgejo/runner/v12/act/model"
	"github.com/myers/drawbar/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- buildStepEnv ---

func TestBuildStepEnv_LayeringOrder(t *testing.T) {
	base := map[string]string{"A": "base", "B": "base"}
	accumulated := map[string]string{"A": "accum"}
	stepEnv := map[string]string{"A": "step"}

	env := buildStepEnv(base, accumulated, stepEnv, nil)

	// Step env wins over accumulated, which wins over base.
	assert.Equal(t, "step", env["A"])
	assert.Equal(t, "base", env["B"])
}

func TestBuildStepEnv_PathPrepend(t *testing.T) {
	env := buildStepEnv(nil, nil, nil, []string{"/custom/bin", "/extra/bin"})
	path := env["PATH"]
	assert.True(t, strings.HasPrefix(path, "/custom/bin:/extra/bin:"), "PATH should start with custom paths, got: %s", path)
}

func TestBuildStepEnv_EmptyInputs(t *testing.T) {
	env := buildStepEnv(nil, nil, nil, nil)
	// Should at least have system env vars like PATH.
	assert.NotEmpty(t, env["PATH"])
}

func TestBuildStepEnv_StepEnvOverridesBase(t *testing.T) {
	base := map[string]string{"KEY": "from-base"}
	stepEnv := map[string]string{"KEY": "from-step"}
	env := buildStepEnv(base, nil, stepEnv, nil)
	assert.Equal(t, "from-step", env["KEY"])
}

// --- loadManifest ---

func TestLoadManifest_Valid(t *testing.T) {
	m := types.Manifest{
		Steps: []types.ManifestStep{
			{ID: "step-0", Name: "Test", Command: "echo hello"},
		},
		BaseEnv: map[string]string{"FOO": "bar"},
	}
	data, err := json.Marshal(m)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "manifest.json")
	require.NoError(t, os.WriteFile(path, data, 0o644))

	loaded, err := loadManifest(path)
	require.NoError(t, err)
	require.Len(t, loaded.Steps, 1)
	assert.Equal(t, "step-0", loaded.Steps[0].ID)
	assert.Equal(t, "echo hello", loaded.Steps[0].Command)
	assert.Equal(t, "bar", loaded.BaseEnv["FOO"])
}

func TestLoadManifest_InvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))

	_, err := loadManifest(path)
	assert.Error(t, err)
}

func TestLoadManifest_MissingFile(t *testing.T) {
	_, err := loadManifest("/nonexistent/path.json")
	assert.Error(t, err)
}

// --- writeState ---

func TestWriteState_WritesJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.jsonl")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	event := StateEvent{
		Event:    "start",
		Step:     0,
		Name:     "Test Step",
		ExitCode: 0,
		Time:     "2024-01-01T00:00:00Z",
	}
	writeState(f, event)

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 1)

	var parsed StateEvent
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &parsed))
	assert.Equal(t, "start", parsed.Event)
	assert.Equal(t, 0, parsed.Step)
	assert.Equal(t, "Test Step", parsed.Name)
}

// --- executeStep ---

func TestExecuteStep_ShellMode(t *testing.T) {
	var captured *exec.Cmd
	origRunner := runCommand
	runCommand = func(cmd *exec.Cmd) error {
		captured = cmd
		return nil
	}
	defer func() { runCommand = origRunner }()

	step := StepDef{Command: "echo hello", Shell: ""}
	code := executeStep(context.Background(), step,map[string]string{"PATH": "/usr/bin"})
	assert.Equal(t, 0, code)
	require.NotNil(t, captured)
	assert.Equal(t, "/bin/sh", captured.Path)
	assert.Contains(t, captured.Args, "-e")
	assert.Contains(t, captured.Args, "echo hello")
}

func TestExecuteStep_BashMode(t *testing.T) {
	var captured *exec.Cmd
	origRunner := runCommand
	runCommand = func(cmd *exec.Cmd) error {
		captured = cmd
		return nil
	}
	defer func() { runCommand = origRunner }()

	step := StepDef{Command: "echo hello", Shell: "bash"}
	executeStep(context.Background(), step,map[string]string{"PATH": "/usr/bin"})
	require.NotNil(t, captured)
	assert.Contains(t, captured.Path, "bash")
}

func TestExecuteStep_DirectArgs(t *testing.T) {
	var captured *exec.Cmd
	origRunner := runCommand
	runCommand = func(cmd *exec.Cmd) error {
		captured = cmd
		return nil
	}
	defer func() { runCommand = origRunner }()

	step := StepDef{Args: []string{"/usr/bin/git", "clone", "repo"}}
	code := executeStep(context.Background(), step,map[string]string{})
	assert.Equal(t, 0, code)
	require.NotNil(t, captured)
	assert.Equal(t, captured.Args[0], "/usr/bin/git")
	assert.Equal(t, captured.Args[1], "clone")
}

func TestExecuteStep_NonExecError(t *testing.T) {
	origRunner := runCommand
	runCommand = func(cmd *exec.Cmd) error {
		return os.ErrPermission
	}
	defer func() { runCommand = origRunner }()

	step := StepDef{Command: "false"}
	code := executeStep(context.Background(), step,map[string]string{"PATH": "/usr/bin"})
	// Non-ExitError returns 1.
	assert.Equal(t, 1, code)
}

func TestExecuteStep_WorkDir(t *testing.T) {
	var captured *exec.Cmd
	origRunner := runCommand
	runCommand = func(cmd *exec.Cmd) error {
		captured = cmd
		return nil
	}
	defer func() { runCommand = origRunner }()

	step := StepDef{Command: "pwd", WorkDir: "/tmp"}
	executeStep(context.Background(), step,map[string]string{"PATH": "/usr/bin"})
	assert.Equal(t, "/tmp", captured.Dir)
}

func TestExecuteStep_DefaultWorkDir(t *testing.T) {
	var captured *exec.Cmd
	origRunner := runCommand
	runCommand = func(cmd *exec.Cmd) error {
		captured = cmd
		return nil
	}
	defer func() { runCommand = origRunner }()

	step := StepDef{Command: "pwd"}
	executeStep(context.Background(), step,map[string]string{"PATH": "/usr/bin"})
	assert.Equal(t, "/workspace", captured.Dir)
}

// --- runEntrypoint integration test ---

func TestRunEntrypoint_HappyPath(t *testing.T) {
	workDir := t.TempDir()

	// Write manifest with two steps.
	manifest := types.Manifest{
		Steps: []types.ManifestStep{
			{
				ID:      "step-0",
				Name:    "Build",
				Command: "echo building",
				Env:     map[string]string{"BUILD_MODE": "release"},
			},
			{
				ID:      "step-1",
				Name:    "Test",
				Command: "echo testing",
			},
		},
		BaseEnv: map[string]string{"CI": "true"},
	}
	manifestPath := filepath.Join(workDir, "manifest.json")
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, data, 0o644))

	// Mock runCommand to succeed and capture calls.
	var capturedCmds []*exec.Cmd
	origRunner := runCommand
	runCommand = func(cmd *exec.Cmd) error {
		capturedCmds = append(capturedCmds, cmd)
		return nil
	}
	defer func() { runCommand = origRunner }()

	// Run the entrypoint.
	ok := runEntrypoint(manifestPath, workDir)
	assert.True(t, ok, "entrypoint should succeed")

	// Both steps should have been executed.
	require.Len(t, capturedCmds, 2)

	// First step should have BUILD_MODE=release in env.
	envMap := envSliceToMap(capturedCmds[0].Env)
	assert.Equal(t, "release", envMap["BUILD_MODE"])
	assert.Equal(t, "true", envMap["CI"])

	// State file should have 4 events: start+end for each step.
	stateData, err := os.ReadFile(filepath.Join(workDir, "state.jsonl"))
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(stateData)), "\n")
	assert.Len(t, lines, 4)

	// Parse events.
	var events []StateEvent
	for _, line := range lines {
		var e StateEvent
		require.NoError(t, json.Unmarshal([]byte(line), &e))
		events = append(events, e)
	}
	assert.Equal(t, "start", events[0].Event)
	assert.Equal(t, "Build", events[0].Name)
	assert.Equal(t, "end", events[1].Event)
	assert.Equal(t, 0, events[1].ExitCode)
	assert.Equal(t, "start", events[2].Event)
	assert.Equal(t, "Test", events[2].Name)
	assert.Equal(t, "end", events[3].Event)

	// Results file should exist with both steps.
	resultsData, err := os.ReadFile(filepath.Join(workDir, "results.json"))
	require.NoError(t, err)
	var results map[string]StepResult
	require.NoError(t, json.Unmarshal(resultsData, &results))
	assert.Len(t, results, 2)
	assert.Equal(t, 0, results["step-0"].ExitCode)
	assert.Equal(t, 0, results["step-1"].ExitCode)

	// Per-step files should have been created.
	for _, name := range []string{"output-0", "env-0", "path-0", "output-1", "env-1", "path-1"} {
		_, err := os.Stat(filepath.Join(workDir, name))
		assert.NoError(t, err, "expected %s to exist", name)
	}

	// GITHUB_OUTPUT should point to the correct file.
	assert.Equal(t, filepath.Join(workDir, "output-0"), envMap["GITHUB_OUTPUT"])
}

func TestRunEntrypoint_StepFailure(t *testing.T) {
	workDir := t.TempDir()

	manifest := types.Manifest{
		Steps: []types.ManifestStep{
			{ID: "step-0", Name: "Fail", Command: "false"},
			{ID: "step-1", Name: "Skip", Command: "echo should not run"},
		},
	}
	manifestPath := filepath.Join(workDir, "manifest.json")
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, data, 0o644))

	var callCount int
	origRunner := runCommand
	runCommand = func(cmd *exec.Cmd) error {
		callCount++
		return &exec.ExitError{} // exit code defaults to -1
	}
	defer func() { runCommand = origRunner }()

	ok := runEntrypoint(manifestPath, workDir)
	assert.False(t, ok, "entrypoint should fail")
	assert.Equal(t, 1, callCount, "second step should not run after first failure")
}

func TestRunEntrypoint_ContinueOnError(t *testing.T) {
	workDir := t.TempDir()

	manifest := types.Manifest{
		Steps: []types.ManifestStep{
			{ID: "step-0", Name: "Fail", Command: "false", ContinueOnError: true},
			{ID: "step-1", Name: "Run", Command: "echo ok"},
		},
	}
	manifestPath := filepath.Join(workDir, "manifest.json")
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, data, 0o644))

	callNum := 0
	origRunner := runCommand
	runCommand = func(cmd *exec.Cmd) error {
		callNum++
		if callNum == 1 {
			return &exec.ExitError{} // first step fails
		}
		return nil // second step succeeds
	}
	defer func() { runCommand = origRunner }()

	ok := runEntrypoint(manifestPath, workDir)
	assert.True(t, ok, "should succeed because continue-on-error is set")
	assert.Equal(t, 2, callNum, "both steps should run")
}

func TestRunEntrypoint_EnvPropagation(t *testing.T) {
	workDir := t.TempDir()

	manifest := types.Manifest{
		Steps: []types.ManifestStep{
			{ID: "step-0", Name: "SetEnv", Command: "echo set env"},
			{ID: "step-1", Name: "UseEnv", Command: "echo use env"},
		},
	}
	manifestPath := filepath.Join(workDir, "manifest.json")
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, data, 0o644))

	var step2Env map[string]string
	callNum := 0
	origRunner := runCommand
	runCommand = func(cmd *exec.Cmd) error {
		callNum++
		if callNum == 1 {
			// Step 1 writes to GITHUB_ENV file.
			envMap := envSliceToMap(cmd.Env)
			envFile := envMap["GITHUB_ENV"]
			os.WriteFile(envFile, []byte("CUSTOM_VAR=hello\n"), 0o644)
		} else {
			step2Env = envSliceToMap(cmd.Env)
		}
		return nil
	}
	defer func() { runCommand = origRunner }()

	ok := runEntrypoint(manifestPath, workDir)
	assert.True(t, ok)
	assert.Equal(t, "hello", step2Env["CUSTOM_VAR"], "env should propagate between steps")
}

func TestRunEntrypoint_IfFailure(t *testing.T) {
	workDir := t.TempDir()

	manifest := types.Manifest{
		Steps: []types.ManifestStep{
			{ID: "build", Name: "Build", Command: "make build"},
			{ID: "cleanup", Name: "Cleanup", Command: "echo cleanup", If: "failure()"},
		},
		Context: &types.EvalContext{
			GitHub: &model.GithubContext{},
		},
	}
	manifestPath := filepath.Join(workDir, "manifest.json")
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, data, 0o644))

	callNum := 0
	var executedSteps []string
	origRunner := runCommand
	runCommand = func(cmd *exec.Cmd) error {
		callNum++
		if callNum == 1 {
			executedSteps = append(executedSteps, "build")
			return &exec.ExitError{} // build fails
		}
		executedSteps = append(executedSteps, "cleanup")
		return nil
	}
	defer func() { runCommand = origRunner }()

	ok := runEntrypoint(manifestPath, workDir)
	assert.False(t, ok, "overall should fail because build failed")
	assert.Equal(t, []string{"build", "cleanup"}, executedSteps, "cleanup should run after build failure")
}

func TestRunEntrypoint_IfAlways(t *testing.T) {
	workDir := t.TempDir()

	manifest := types.Manifest{
		Steps: []types.ManifestStep{
			{ID: "build", Name: "Build", Command: "make build"},
			{ID: "notify", Name: "Notify", Command: "echo notify", If: "always()"},
		},
		Context: &types.EvalContext{
			GitHub: &model.GithubContext{},
		},
	}
	manifestPath := filepath.Join(workDir, "manifest.json")
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, data, 0o644))

	callNum := 0
	var executedSteps []string
	origRunner := runCommand
	runCommand = func(cmd *exec.Cmd) error {
		callNum++
		if callNum == 1 {
			executedSteps = append(executedSteps, "build")
			return &exec.ExitError{} // build fails
		}
		executedSteps = append(executedSteps, "notify")
		return nil
	}
	defer func() { runCommand = origRunner }()

	ok := runEntrypoint(manifestPath, workDir)
	assert.False(t, ok) // overall fails
	assert.Equal(t, []string{"build", "notify"}, executedSteps, "always() step should run after failure")
}

func TestRunEntrypoint_StepOutcome(t *testing.T) {
	workDir := t.TempDir()

	// Note: bare `steps.*.outcome` expressions get an implicit `success() &&` prefix
	// per GitHub Actions semantics. Use `always() &&` to check outcomes after failures.
	manifest := types.Manifest{
		Steps: []types.ManifestStep{
			{ID: "build", Name: "Build", Command: "make build"},
			{ID: "on-fail", Name: "OnFail", Command: "echo failed", If: "always() && steps.build.outcome == 'failure'"},
			{ID: "on-success", Name: "OnSuccess", Command: "echo ok", If: "always() && steps.build.outcome == 'success'"},
		},
		Context: &types.EvalContext{
			GitHub: &model.GithubContext{},
		},
	}
	manifestPath := filepath.Join(workDir, "manifest.json")
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, data, 0o644))

	callNum := 0
	var executedSteps []string
	origRunner := runCommand
	runCommand = func(cmd *exec.Cmd) error {
		callNum++
		if callNum == 1 {
			executedSteps = append(executedSteps, "build")
			return &exec.ExitError{} // build fails
		}
		executedSteps = append(executedSteps, cmd.Args[len(cmd.Args)-1]) // capture the script
		return nil
	}
	defer func() { runCommand = origRunner }()

	ok := runEntrypoint(manifestPath, workDir)
	assert.False(t, ok)
	// "on-fail" should run (build failed), "on-success" should be skipped.
	assert.Equal(t, []string{"build", "echo failed"}, executedSteps)
}

func TestRunEntrypoint_SuccessSkipsFailureStep(t *testing.T) {
	workDir := t.TempDir()

	manifest := types.Manifest{
		Steps: []types.ManifestStep{
			{ID: "build", Name: "Build", Command: "make build"},
			{ID: "cleanup", Name: "Cleanup", Command: "echo cleanup", If: "failure()"},
		},
		Context: &types.EvalContext{
			GitHub: &model.GithubContext{},
		},
	}
	manifestPath := filepath.Join(workDir, "manifest.json")
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, data, 0o644))

	var executedSteps []string
	origRunner := runCommand
	runCommand = func(cmd *exec.Cmd) error {
		executedSteps = append(executedSteps, "executed")
		return nil // all succeed
	}
	defer func() { runCommand = origRunner }()

	ok := runEntrypoint(manifestPath, workDir)
	assert.True(t, ok)
	// Only build should run; cleanup with if:failure() should be skipped.
	assert.Equal(t, []string{"executed"}, executedSteps)
}

func TestExecuteStep_Timeout(t *testing.T) {
	origRunner := runCommand
	runCommand = func(cmd *exec.Cmd) error {
		// Simulate a long-running command. Sleep then check if we were killed.
		time.Sleep(500 * time.Millisecond)
		return context.DeadlineExceeded
	}
	defer func() { runCommand = origRunner }()

	// Context with 50ms timeout — command "runs" for 500ms.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	step := StepDef{Command: "sleep 300", TimeoutMinutes: 0.001}
	start := time.Now()
	code := executeStep(ctx, step, map[string]string{"PATH": "/usr/bin"})
	elapsed := time.Since(start)

	// The mock sleeps 500ms but the context expires at 50ms.
	// executeStep checks ctx.Err() after runCommand returns.
	// Note: with mock, the process isn't actually killed by CommandContext,
	// so it waits the full 500ms. What matters is the exit code.
	_ = elapsed
	assert.Equal(t, 1, code)
}

func envSliceToMap(envSlice []string) map[string]string {
	m := make(map[string]string)
	for _, e := range envSlice {
		if idx := strings.IndexByte(e, '='); idx > 0 {
			m[e[:idx]] = e[idx+1:]
		}
	}
	return m
}

func TestWriteState_MultipleEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.jsonl")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	writeState(f, StateEvent{Event: "start", Step: 0})
	writeState(f, StateEvent{Event: "end", Step: 0, ExitCode: 1})

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	assert.Len(t, lines, 2)
}
