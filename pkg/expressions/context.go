package expressions

import (
	"strings"

	"code.forgejo.org/forgejo/runner/v12/act/exprparser"
	"code.forgejo.org/forgejo/runner/v12/act/model"
	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
)

// BuildGithubContext extracts a GithubContext from the task's Context protobuf struct.
func BuildGithubContext(task *runnerv1.Task) *model.GithubContext {
	ctx := task.GetContext()
	if ctx == nil {
		return &model.GithubContext{}
	}
	f := ctx.GetFields()

	get := func(key string) string {
		if v, ok := f[key]; ok {
			return v.GetStringValue()
		}
		return ""
	}

	ghc := &model.GithubContext{
		RunID:           get("run_id"),
		RunNumber:       get("run_number"),
		RunAttempt:      get("run_attempt"),
		Actor:           get("actor"),
		Repository:      get("repository"),
		EventName:       get("event_name"),
		Sha:             get("sha"),
		Ref:             get("ref"),
		RefName:         get("ref_name"),
		RefType:         get("ref_type"),
		HeadRef:         get("head_ref"),
		BaseRef:         get("base_ref"),
		Token:           get("token"),
		RepositoryOwner: get("repository_owner"),
		RetentionDays:   get("retention_days"),
		ServerURL:       strings.TrimRight(get("server_url"), "/"),
		APIURL:          get("api_url"),
		Workspace:       "/workspace",
	}

	// Event payload is a nested struct — extract if available.
	if v, ok := f["event"]; ok && v.GetStructValue() != nil {
		ghc.Event = v.GetStructValue().AsMap()
	}

	// Use FORGEJO_TOKEN or GITEA_TOKEN from secrets if available.
	if t, ok := task.Secrets["FORGEJO_TOKEN"]; ok && t != "" {
		ghc.Token = t
	} else if t, ok := task.Secrets["GITEA_TOKEN"]; ok && t != "" {
		ghc.Token = t
	} else if t, ok := task.Secrets["GITHUB_TOKEN"]; ok && t != "" {
		ghc.Token = t
	}

	return ghc
}

// BuildEnvironment creates a full EvaluationEnvironment from a Forgejo task.
func BuildEnvironment(task *runnerv1.Task, jobEnv map[string]string) *exprparser.EvaluationEnvironment {
	ghc := BuildGithubContext(task)

	// Build needs from task.
	needs := make(map[string]exprparser.Needs)
	for id, need := range task.GetNeeds() {
		needs[id] = exprparser.Needs{
			Outputs: need.GetOutputs(),
			Result:  strings.ToLower(strings.TrimPrefix(need.GetResult().String(), "RESULT_")),
		}
	}

	return &exprparser.EvaluationEnvironment{
		Github:  ghc,
		Env:     jobEnv,
		Job: &model.JobContext{
			Status: "success", // Default: no failures yet.
		},
		Secrets: task.GetSecrets(),
		Vars:    task.GetVars(),
		Needs:   needs,
		Steps:   make(map[string]*model.StepResult),
		Runner: map[string]any{
			"os":   "linux",
			"arch": "x64",
			"name": "drawbar",
		},
	}
}
