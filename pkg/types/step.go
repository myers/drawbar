package types

import (
	"code.forgejo.org/forgejo/runner/v12/act/exprparser"
	"code.forgejo.org/forgejo/runner/v12/act/model"
)

// StepSpec describes a single workflow step.
// Used by pkg/actions, pkg/k8s, and pkg/workflow.
type StepSpec struct {
	ID              string
	Name            string
	Shell           string            // "sh", "bash", "python"
	Script          string
	Args            []string          // direct exec mode (no shell); mutually exclusive with Script
	Env             map[string]string
	Image           string            // optional image override (for Docker actions)
	Cmd             []string          // optional command override (for Docker actions)
	ActionDir       string            // if set, mount this action from cache PVC
	ContinueOnError bool
	If              string  // raw if: expression for runtime evaluation
	TimeoutMinutes  float64 // 0 means no per-step timeout
}

// Manifest is the JSON structure the entrypoint binary reads.
type Manifest struct {
	Steps   []ManifestStep    `json:"steps"`
	BaseEnv map[string]string `json:"baseEnv"`
	Context *EvalContext      `json:"context,omitempty"`
}

// EvalContext holds the serializable evaluation context for runtime if: conditions.
// This is a subset of exprparser.EvaluationEnvironment — only the JSON-safe fields.
// The full environment is reconstructed in the entrypoint via BuildEvalEnv.
type EvalContext struct {
	GitHub  *model.GithubContext        `json:"github"`
	Env     map[string]string           `json:"env"`
	Secrets map[string]string           `json:"secrets"`
	Vars    map[string]string           `json:"vars"`
	Needs   map[string]exprparser.Needs `json:"needs"`
}

// BuildEvalEnv reconstructs an exprparser.EvaluationEnvironment from the serialized context.
func (c *EvalContext) BuildEvalEnv() *exprparser.EvaluationEnvironment {
	needs := make(map[string]exprparser.Needs)
	for k, v := range c.Needs {
		needs[k] = v
	}
	return &exprparser.EvaluationEnvironment{
		Github:  c.GitHub,
		Env:     c.Env,
		Secrets: c.Secrets,
		Vars:    c.Vars,
		Needs:   needs,
		Steps:   make(map[string]*model.StepResult),
		Job:     &model.JobContext{Status: "success"},
		Runner:  map[string]any{"os": "linux", "arch": "x64", "name": "drawbar"},
	}
}

// ManifestStep describes a step in the manifest.
type ManifestStep struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Command         string            `json:"command"`
	Args            []string          `json:"args,omitempty"` // direct exec mode (no shell)
	Shell           string            `json:"shell"`          // "sh", "bash", "python"
	Env             map[string]string `json:"env"`
	WorkDir         string            `json:"workdir"`
	ContinueOnError bool              `json:"continueOnError"`
	If              string            `json:"if,omitempty"`       // raw expression for runtime evaluation
	TimeoutMinutes  float64           `json:"timeoutMinutes,omitempty"` // 0 means no per-step timeout
}

// StateEvent is written to state.jsonl for the controller to track step lifecycle.
// Event values: "start", "end", "skip".
type StateEvent struct {
	Event    string `json:"event"`
	Step     int    `json:"step"`
	Name     string `json:"name,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Time     string `json:"time"`
}

// StepResult records the outcome of a step.
type StepResult struct {
	ExitCode int               `json:"exitCode"`
	Outputs  map[string]string `json:"outputs"`
}
