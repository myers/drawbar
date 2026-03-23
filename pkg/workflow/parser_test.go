package workflow

import (
	"testing"

	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTask(t *testing.T) {
	payload := []byte(`name: Test
on: [push]
jobs:
  hello:
    runs-on: ubuntu-latest
    steps:
      - run: echo "hello world"
      - run: echo "second step"
`)

	task := &runnerv1.Task{
		Id:              1,
		WorkflowPayload: payload,
	}

	parsed, err := ParseTask(task)
	require.NoError(t, err)

	assert.Equal(t, "hello", parsed.JobID)
	assert.Equal(t, []string{"ubuntu-latest"}, parsed.RunsOn)
	assert.Len(t, parsed.Steps, 2)
	assert.Equal(t, `echo "hello world"`, parsed.Steps[0].Run)
	assert.Equal(t, `echo "second step"`, parsed.Steps[1].Run)
}

func TestParseTask_EmptyPayload(t *testing.T) {
	task := &runnerv1.Task{Id: 1}
	_, err := ParseTask(task)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty workflow payload")
}

func TestParseTask_InvalidYAML(t *testing.T) {
	task := &runnerv1.Task{
		Id:              1,
		WorkflowPayload: []byte("not: valid: yaml: ["),
	}
	_, err := ParseTask(task)
	assert.Error(t, err)
}

func TestParseTask_WithName(t *testing.T) {
	payload := []byte(`name: Build
on: [push]
jobs:
  build:
    name: Build Project
    runs-on: ubuntu-latest
    steps:
      - run: make build
`)

	task := &runnerv1.Task{
		Id:              1,
		WorkflowPayload: payload,
	}

	parsed, err := ParseTask(task)
	require.NoError(t, err)
	assert.Equal(t, "build", parsed.JobID)
	assert.Equal(t, "Build Project", parsed.Name)
	assert.Equal(t, "make build", parsed.Steps[0].Run)
}
