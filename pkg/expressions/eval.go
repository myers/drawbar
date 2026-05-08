package expressions

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/nektos/act/pkg/exprparser"
	"github.com/nektos/act/pkg/model"
)

// Evaluator wraps exprparser for evaluating GitHub Actions expressions.
type Evaluator struct {
	interp exprparser.Interpreter
	env    *exprparser.EvaluationEnvironment
}

// NewEvaluator creates an evaluator from an EvaluationEnvironment.
func NewEvaluator(env *exprparser.EvaluationEnvironment) *Evaluator {
	return &Evaluator{
		interp: exprparser.NewInterpeter(env, exprparser.Config{
			Context: "step", // Required for success()/failure()/cancelled() functions.
		}),
		env: env,
	}
}

// Interpolate replaces all ${{ }} expressions in a string with their evaluated values.
// For pure expressions like "${{ github.sha }}", returns the evaluated value.
// For mixed strings like "sha=${{ github.sha }}", evaluates each expression inline.
func (e *Evaluator) Interpolate(input string) string {
	if !strings.Contains(input, "${{") || !strings.Contains(input, "}}") {
		return input
	}

	// Use RewriteSubExpression to handle mixed text + expressions.
	expr := rewriteSubExpression(input, true)
	evaluated, err := e.interp.Evaluate(expr, exprparser.DefaultStatusCheckNone)
	if err != nil {
		slog.Warn("expression evaluation failed", "input", input, "error", err)
		return input // Return original on error.
	}

	result, ok := evaluated.(string)
	if !ok {
		return fmt.Sprintf("%v", evaluated)
	}
	return result
}

// EvalCondition evaluates a step if: condition.
// Returns true if the step should run, false if it should be skipped.
// An empty condition defaults to success() (step runs only if all previous steps succeeded).
func (e *Evaluator) EvalCondition(expr string) (bool, error) {
	if expr == "" {
		// Default: run if previous steps succeeded.
		expr = "success()"
	}

	// Strip ${{ }} wrapper if present.
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "${{") && strings.HasSuffix(expr, "}}") {
		expr = strings.TrimPrefix(expr, "${{")
		expr = strings.TrimSuffix(expr, "}}")
		expr = strings.TrimSpace(expr)
	}

	rewritten := rewriteSubExpression(expr, false)
	evaluated, err := e.interp.Evaluate(rewritten, exprparser.DefaultStatusCheckSuccess)
	if err != nil {
		return false, fmt.Errorf("evaluating condition %q: %w", expr, err)
	}

	return exprparser.IsTruthy(evaluated), nil
}

// InterpolateMap interpolates all values in a string map.
func (e *Evaluator) InterpolateMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return m
	}
	result := make(map[string]string, len(m))
	for k, v := range m {
		result[k] = e.Interpolate(v)
	}
	return result
}

// SetStepResult records a completed step's result for future ${{ steps.* }}
// references. Outcome is the raw step exit status (success or failure);
// Conclusion applies the continueOnError modifier — a failed step with
// continueOnError=true records outcome=failure but conclusion=success, so
// downstream ${{ failure() }} / ${{ success() }} (which consult conclusion)
// don't trip on a swallowed failure. See GitHub Actions docs for the
// outcome-vs-conclusion split.
func (e *Evaluator) SetStepResult(stepID string, outcome string, continueOnError bool, outputs map[string]string) {
	if e.env.Steps == nil {
		e.env.Steps = make(map[string]*model.StepResult)
	}
	outcomeStatus := model.StepStatusSuccess
	conclusionStatus := model.StepStatusSuccess
	if outcome == "failure" {
		outcomeStatus = model.StepStatusFailure
		if !continueOnError {
			conclusionStatus = model.StepStatusFailure
		}
	}
	e.env.Steps[stepID] = &model.StepResult{
		Outcome:    outcomeStatus,
		Conclusion: conclusionStatus,
		Outputs:    outputs,
	}
}

// SetJobStatus updates the job status in the evaluation context.
// Used by the entrypoint to set "failure" after a step fails,
// so that failure()/success() evaluate correctly for subsequent steps.
func (e *Evaluator) SetJobStatus(status string) {
	if e.env.Job != nil {
		e.env.Job.Status = status
	}
}

// UpdateEnv merges additional environment variables into the evaluation context.
func (e *Evaluator) UpdateEnv(env map[string]string) {
	for k, v := range env {
		e.env.Env[k] = v
	}
}
