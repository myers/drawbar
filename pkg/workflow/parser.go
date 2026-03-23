package workflow

import (
	"bytes"
	"fmt"

	"code.forgejo.org/forgejo/runner/v12/act/model"
	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
)

// ParsedJob contains the extracted job information from a task's workflow payload.
type ParsedJob struct {
	JobID     string
	Name      string
	RunsOn    []string
	Steps     []*model.Step
	Env       map[string]string
	Container *model.ContainerSpec
	Services  map[string]*model.ContainerSpec
	Workflow  *model.Workflow
}

// ParseTask extracts the single job from a Forgejo task's workflow payload.
// Forgejo sends exactly one job per task — the server pre-selects which job to run.
func ParseTask(task *runnerv1.Task) (*ParsedJob, error) {
	if len(task.GetWorkflowPayload()) == 0 {
		return nil, fmt.Errorf("task %d has empty workflow payload", task.GetId())
	}

	workflow, err := model.ReadWorkflow(bytes.NewReader(task.GetWorkflowPayload()), true)
	if err != nil {
		return nil, fmt.Errorf("parsing workflow: %w", err)
	}

	jobIDs := workflow.GetJobIDs()
	if len(jobIDs) == 0 {
		return nil, fmt.Errorf("workflow has no jobs")
	}
	if len(jobIDs) != 1 {
		return nil, fmt.Errorf("expected 1 job, got %d: %v", len(jobIDs), jobIDs)
	}

	jobID := jobIDs[0]
	job := workflow.GetJob(jobID)
	if job == nil {
		return nil, fmt.Errorf("job %q not found in workflow", jobID)
	}

	return &ParsedJob{
		JobID:     jobID,
		Name:      job.Name,
		RunsOn:    job.RunsOn(),
		Steps:     job.Steps,
		Env:       job.Environment(),
		Container: job.Container(),
		Services:  job.Services,
		Workflow:  workflow,
	}, nil
}
