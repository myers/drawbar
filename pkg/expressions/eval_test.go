package expressions

import (
	"testing"

	"code.forgejo.org/forgejo/runner/v12/act/exprparser"
	"code.forgejo.org/forgejo/runner/v12/act/model"
	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
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

	eval.SetStepResult("build", "success", map[string]string{"artifact": "myapp.tar"})

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
	eval.SetStepResult("deploy", "failure", nil)

	assert.Equal(t, model.StepStatusFailure, env.Steps["deploy"].Conclusion)
	assert.Equal(t, model.StepStatusFailure, env.Steps["deploy"].Outcome)
}
