package expressions

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/nektos/act/pkg/exprparser"
	"github.com/nektos/act/pkg/model"
	runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

func makeTask() *runnerv1.Task {
	fields := map[string]*structpb.Value{
		"repository":       structpb.NewStringValue("TestingAdmin/test-runner"),
		"sha":              structpb.NewStringValue("abc123def"),
		"ref":              structpb.NewStringValue("refs/heads/main"),
		"ref_name":         structpb.NewStringValue("main"),
		"ref_type":         structpb.NewStringValue("branch"),
		"actor":            structpb.NewStringValue("TestingAdmin"),
		"repository_owner": structpb.NewStringValue("TestingAdmin"),
		"event_name":       structpb.NewStringValue("push"),
		"server_url":       structpb.NewStringValue("http://localhost:8080/"),
		"api_url":          structpb.NewStringValue("http://localhost:8080/api/v1"),
		"run_id":           structpb.NewStringValue("42"),
		"run_number":       structpb.NewStringValue("7"),
		"token":            structpb.NewStringValue("test-token"),
	}
	return &runnerv1.Task{
		Id:      1,
		Context: &structpb.Struct{Fields: fields},
		Secrets: map[string]string{"MY_SECRET": "secret-value"},
		Vars:    map[string]string{"MY_VAR": "var-value"},
	}
}

func TestBuildGithubContext(t *testing.T) {
	task := makeTask()
	ghc := BuildGithubContext(task)

	assert.Equal(t, "TestingAdmin/test-runner", ghc.Repository)
	assert.Equal(t, "abc123def", ghc.Sha)
	assert.Equal(t, "refs/heads/main", ghc.Ref)
	assert.Equal(t, "main", ghc.RefName)
	assert.Equal(t, "TestingAdmin", ghc.Actor)
	assert.Equal(t, "push", ghc.EventName)
	assert.Equal(t, "http://localhost:8080", ghc.ServerURL)
	assert.Equal(t, "/workspace", ghc.Workspace)
}

func TestInterpolate_GithubContext(t *testing.T) {
	task := makeTask()
	env := BuildEnvironment(task, nil)
	eval := NewEvaluator(env)

	assert.Equal(t, "abc123def", eval.Interpolate("${{ github.sha }}"))
	assert.Equal(t, "TestingAdmin", eval.Interpolate("${{ github.actor }}"))
	assert.Equal(t, "TestingAdmin/test-runner", eval.Interpolate("${{ github.repository }}"))
	assert.Equal(t, "main", eval.Interpolate("${{ github.ref_name }}"))
}

func TestInterpolate_MixedText(t *testing.T) {
	task := makeTask()
	env := BuildEnvironment(task, nil)
	eval := NewEvaluator(env)

	result := eval.Interpolate("Hello ${{ github.actor }}, sha=${{ github.sha }}")
	assert.Equal(t, "Hello TestingAdmin, sha=abc123def", result)
}

func TestInterpolate_EnvVars(t *testing.T) {
	task := makeTask()
	env := BuildEnvironment(task, map[string]string{"GREETING": "hello"})
	eval := NewEvaluator(env)

	assert.Equal(t, "hello", eval.Interpolate("${{ env.GREETING }}"))
}

func TestInterpolate_NoExpression(t *testing.T) {
	task := makeTask()
	env := BuildEnvironment(task, nil)
	eval := NewEvaluator(env)

	assert.Equal(t, "plain text", eval.Interpolate("plain text"))
	assert.Equal(t, "", eval.Interpolate(""))
}

func TestInterpolate_Vars(t *testing.T) {
	task := makeTask()
	env := BuildEnvironment(task, nil)
	eval := NewEvaluator(env)

	assert.Equal(t, "var-value", eval.Interpolate("${{ vars.MY_VAR }}"))
}

func TestInterpolate_Secrets(t *testing.T) {
	task := makeTask()
	env := BuildEnvironment(task, nil)
	eval := NewEvaluator(env)

	assert.Equal(t, "secret-value", eval.Interpolate("${{ secrets.MY_SECRET }}"))
}

func TestEvalCondition_True(t *testing.T) {
	task := makeTask()
	env := BuildEnvironment(task, nil)
	eval := NewEvaluator(env)

	result, err := eval.EvalCondition("github.ref_name == 'main'")
	require.NoError(t, err)
	assert.True(t, result)
}

func TestEvalCondition_False(t *testing.T) {
	task := makeTask()
	env := BuildEnvironment(task, nil)
	eval := NewEvaluator(env)

	result, err := eval.EvalCondition("github.ref_name == 'develop'")
	require.NoError(t, err)
	assert.False(t, result)
}

func TestEvalCondition_WithBraces(t *testing.T) {
	task := makeTask()
	env := BuildEnvironment(task, nil)
	eval := NewEvaluator(env)

	result, err := eval.EvalCondition("${{ github.event_name == 'push' }}")
	require.NoError(t, err)
	assert.True(t, result)
}

func TestEvalCondition_Empty_DefaultsToSuccess(t *testing.T) {
	task := makeTask()
	env := BuildEnvironment(task, nil)
	eval := NewEvaluator(env)

	// Empty condition defaults to success() — which is true when no steps have failed.
	result, err := eval.EvalCondition("")
	require.NoError(t, err)
	assert.True(t, result)
}

func TestEvalCondition_Always(t *testing.T) {
	task := makeTask()
	env := BuildEnvironment(task, nil)
	eval := NewEvaluator(env)

	result, err := eval.EvalCondition("always()")
	require.NoError(t, err)
	assert.True(t, result)
}

func TestEvalCondition_Contains(t *testing.T) {
	task := makeTask()
	env := BuildEnvironment(task, nil)
	eval := NewEvaluator(env)

	result, err := eval.EvalCondition("contains(github.repository, 'test-runner')")
	require.NoError(t, err)
	assert.True(t, result)

	result, err = eval.EvalCondition("contains(github.repository, 'nonexistent')")
	require.NoError(t, err)
	assert.False(t, result)
}

func TestInterpolateMap(t *testing.T) {
	task := makeTask()
	env := BuildEnvironment(task, nil)
	eval := NewEvaluator(env)

	m := map[string]string{
		"REPO":  "${{ github.repository }}",
		"PLAIN": "no-expression",
	}
	result := eval.InterpolateMap(m)
	assert.Equal(t, "TestingAdmin/test-runner", result["REPO"])
	assert.Equal(t, "no-expression", result["PLAIN"])
}

func TestSetStepResult(t *testing.T) {
	env := &exprparser.EvaluationEnvironment{
		Github: &model.GithubContext{},
		Steps:  make(map[string]*model.StepResult),
	}
	eval := NewEvaluator(env)

	eval.SetStepResult("build", "success", false, map[string]string{"artifact": "myapp.tar"})

	assert.NotNil(t, env.Steps["build"])
	assert.Equal(t, model.StepStatusSuccess, env.Steps["build"].Conclusion)
	assert.Equal(t, "myapp.tar", env.Steps["build"].Outputs["artifact"])
}

func TestUpdateEnv(t *testing.T) {
	env := &exprparser.EvaluationEnvironment{
		Github: &model.GithubContext{},
		Env:    map[string]string{"EXISTING": "val"},
	}
	eval := NewEvaluator(env)
	eval.UpdateEnv(map[string]string{"NEW_KEY": "new_val", "EXISTING": "overridden"})

	assert.Equal(t, "new_val", env.Env["NEW_KEY"])
	assert.Equal(t, "overridden", env.Env["EXISTING"])
}

func TestSetStepResult_Failure(t *testing.T) {
	env := &exprparser.EvaluationEnvironment{
		Github: &model.GithubContext{},
		Steps:  make(map[string]*model.StepResult),
	}
	eval := NewEvaluator(env)
	eval.SetStepResult("deploy", "failure", false, nil)

	assert.Equal(t, model.StepStatusFailure, env.Steps["deploy"].Conclusion)
	assert.Equal(t, model.StepStatusFailure, env.Steps["deploy"].Outcome)
}

// TestSetStepResult_OutcomeAndConclusionTable is the bug 020 finding-B
// regression. Per GitHub Actions spec, Outcome reflects the raw step exit
// status and Conclusion applies continue-on-error: a failure with
// continue-on-error: true is recorded as outcome=failure, conclusion=success.
// Drawbar previously conflated the two and ignored continue-on-error here.
func TestSetStepResult_OutcomeAndConclusionTable(t *testing.T) {
	cases := []struct {
		name            string
		outcome         string
		continueOnError bool
		wantOutcome     string
		wantConclusion  string
	}{
		{"success without continue-on-error", "success", false, "success", "success"},
		{"success with continue-on-error", "success", true, "success", "success"},
		{"failure without continue-on-error", "failure", false, "failure", "failure"},
		{"failure with continue-on-error masks conclusion", "failure", true, "failure", "success"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := &exprparser.EvaluationEnvironment{
				Github: &model.GithubContext{},
				Steps:  make(map[string]*model.StepResult),
			}
			eval := NewEvaluator(env)
			eval.SetStepResult("step", tc.outcome, tc.continueOnError, nil)

			// model.StepStatus is unexported; compare via the exported
			// constants' String() output.
			assert.Equal(t, tc.wantOutcome, env.Steps["step"].Outcome.String(), "outcome")
			assert.Equal(t, tc.wantConclusion, env.Steps["step"].Conclusion.String(), "conclusion")
		})
	}
}

// TestSetStepResult_FailureFunctionRespectsConclusion locks in the
// downstream consequence of finding B: ${{ failure() }} must consult
// conclusion (continue-on-error masked), not raw outcome. After a failed
// step with continue-on-error: true, a subsequent step's `if: failure()`
// should evaluate to false — the prior failure was swallowed.
func TestSetStepResult_FailureFunctionRespectsConclusion(t *testing.T) {
	env := &exprparser.EvaluationEnvironment{
		Github: &model.GithubContext{},
		Job:    &model.JobContext{Status: "success"},
		Steps:  make(map[string]*model.StepResult),
	}
	eval := NewEvaluator(env)

	// Step 1 fails but continue-on-error: true → conclusion is success and
	// the entrypoint correspondingly does NOT call SetJobStatus("failure").
	eval.SetStepResult("flaky", "failure", true, nil)

	// Step 2's `if: failure()` should NOT trip.
	tripped, err := eval.EvalCondition("failure()")
	require.NoError(t, err)
	assert.False(t, tripped,
		"failure() must not trip when the only failed step had continue-on-error: true")
}

// TestSetStepResult_UnknownOutcomeWarnsAndDefaultsToSuccess hardens the
// outcome-string contract. Today's only callers pass "success" or
// "failure"; anything else (a future "skipped" path, a typo, a refactor
// that forgets to extend the function) must NOT silently land as success
// without surfacing the divergence. Behavior on unknown input: warn-log
// the offending value AND default to success (preserving prior behavior
// so an unknown value can't fail a successful job).
func TestSetStepResult_UnknownOutcomeWarnsAndDefaultsToSuccess(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	env := &exprparser.EvaluationEnvironment{
		Github: &model.GithubContext{},
		Steps:  make(map[string]*model.StepResult),
	}
	eval := NewEvaluator(env)
	eval.SetStepResult("weird", "skipped", false, nil)

	assert.Equal(t, "success", env.Steps["weird"].Outcome.String(),
		"unknown outcome must default to success (preserving prior behavior)")
	assert.Equal(t, "success", env.Steps["weird"].Conclusion.String())
	assert.Contains(t, buf.String(), "unknown step outcome",
		"unknown outcome must surface a warn log; got: %q", buf.String())
	assert.Contains(t, buf.String(), "skipped",
		"warn log must include the offending value; got: %q", buf.String())
}
