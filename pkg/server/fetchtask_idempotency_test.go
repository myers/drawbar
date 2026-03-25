package server

// These tests demonstrate a bug in Gitea's FetchTask RPC: when a task is
// assigned to a runner but the response is lost (network error), retrying
// with the same x-runner-request-key returns "no task" because the server
// does not implement idempotency. The task is permanently lost.
//
// A companion test shows what correct behavior looks like: the server
// detects the duplicate request key and returns the previously-assigned
// task with a regenerated runtime token.
//
// See GITEA_FETCHTASK_BUG.md for the full bug description.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
	"code.gitea.io/actions-proto-go/runner/v1/runnerv1connect"
	"connectrpc.com/connect"
	gouuid "github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

// giteaFetchHandler simulates Gitea's current FetchTask behavior:
// - First call with a pending task: assigns it and returns it
// - Second call (retry with same request key): returns empty (task already assigned)
//
// A correct implementation would detect the retry via the request key and
// return the previously-assigned task with a regenerated token.
type giteaFetchHandler struct {
	mu          sync.Mutex
	pendingTask *runnerv1.Task // task waiting to be assigned
	assignedTo  string         // request key that consumed the task
	fetchCalls  int
	lastKey     string
}

func (h *giteaFetchHandler) serveMux(prefix string) *http.ServeMux {
	mux := http.NewServeMux()

	mux.Handle(prefix+runnerv1connect.RunnerServiceFetchTaskProcedure, connect.NewUnaryHandler(
		runnerv1connect.RunnerServiceFetchTaskProcedure,
		func(_ context.Context, req *connect.Request[runnerv1.FetchTaskRequest]) (*connect.Response[runnerv1.FetchTaskResponse], error) {
			h.mu.Lock()
			defer h.mu.Unlock()

			h.fetchCalls++
			h.lastKey = req.Header().Get("x-runner-request-key")

			// Gitea's behavior: if there's a pending task, assign it.
			// If the task was already assigned (even to the same request key), return empty.
			if h.pendingTask != nil && h.assignedTo == "" {
				h.assignedTo = h.lastKey
				task := h.pendingTask
				h.pendingTask = nil
				return connect.NewResponse(&runnerv1.FetchTaskResponse{
					Task:         task,
					TasksVersion: 1,
				}), nil
			}

			// No pending task (or already assigned) — return empty.
			return connect.NewResponse(&runnerv1.FetchTaskResponse{
				TasksVersion: 1,
			}), nil
		},
	))

	return mux
}

func TestFetchTask_IdempotencyBug(t *testing.T) {
	// Set up a mock Gitea server with one pending task.
	handler := &giteaFetchHandler{
		pendingTask: &runnerv1.Task{
			Id: 42,
			WorkflowPayload: []byte(`name: Test
on: [push]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo hello
`),
			Context: &structpb.Struct{
				Fields: map[string]*structpb.Value{
					"gitea_runtime_token": structpb.NewStringValue("runtime-token-42"),
				},
			},
		},
	}

	server := httptest.NewServer(handler.serveMux("/api/actions"))
	defer server.Close()

	client := NewClient(server.URL, false, "test-uuid", "test-token", time.Second, 5*time.Second)

	// First FetchTask: should receive the task.
	requestKey := gouuid.New()
	cleanup := client.SetRequestKey(requestKey)

	resp, err := client.FetchTask(context.Background(), connect.NewRequest(&runnerv1.FetchTaskRequest{
		TasksVersion: 0,
	}))
	cleanup()
	require.NoError(t, err)
	require.NotNil(t, resp.Msg.GetTask())
	assert.Equal(t, int64(42), resp.Msg.GetTask().GetId())

	// Simulate network failure: the server assigned the task, but pretend
	// the response was lost. The runner retries with the SAME request key
	// (the act_runner protocol retains the key until a successful response).

	// Second FetchTask with the same request key.
	cleanup = client.SetRequestKey(requestKey) // same key — this is a retry
	resp2, err := client.FetchTask(context.Background(), connect.NewRequest(&runnerv1.FetchTaskRequest{
		TasksVersion: 0,
	}))
	cleanup()
	require.NoError(t, err)

	// BUG: Gitea returns empty — the task was already assigned but the server
	// doesn't recognize the retry. The task is now lost.
	//
	// A correct server would detect the duplicate request key and return the
	// same task (with a regenerated runtime token, since the original is
	// stored as a one-way hash).
	task2 := resp2.Msg.GetTask()
	isTaskLost := task2 == nil || task2.GetId() == 0

	assert.True(t, isTaskLost,
		"BUG CONFIRMED: Gitea does not implement FetchTask idempotency. "+
			"When a task assignment response is lost and the runner retries with "+
			"the same x-runner-request-key, the server returns empty instead of "+
			"the previously-assigned task. See GITEA_FETCHTASK_BUG.md.")

	// Verify the server saw both calls with the same request key.
	handler.mu.Lock()
	assert.Equal(t, 2, handler.fetchCalls)
	assert.Equal(t, requestKey.String(), handler.assignedTo,
		"task was assigned to our request key")
	handler.mu.Unlock()
}

// TestFetchTask_WithIdempotency shows what correct behavior looks like:
// the server returns the same task on retry with the same request key.
func TestFetchTask_WithIdempotency(t *testing.T) {
	// This handler implements the proposed fix: it tracks assigned tasks
	// by request key and returns them on retry.
	type assignment struct {
		task *runnerv1.Task
		key  string
	}

	var (
		mu          sync.Mutex
		pending     *runnerv1.Task
		assignments []assignment
	)

	pending = &runnerv1.Task{
		Id: 99,
		Context: &structpb.Struct{
			Fields: map[string]*structpb.Value{
				"gitea_runtime_token": structpb.NewStringValue("token-99"),
			},
		},
	}

	mux := http.NewServeMux()
	mux.Handle("/api/actions"+runnerv1connect.RunnerServiceFetchTaskProcedure, connect.NewUnaryHandler(
		runnerv1connect.RunnerServiceFetchTaskProcedure,
		func(_ context.Context, req *connect.Request[runnerv1.FetchTaskRequest]) (*connect.Response[runnerv1.FetchTaskResponse], error) {
			mu.Lock()
			defer mu.Unlock()

			key := req.Header().Get("x-runner-request-key")

			// Check for existing assignment with same key (idempotent recovery).
			for _, a := range assignments {
				if a.key == key {
					// Return the same task with a regenerated token.
					recovered := *a.task
					ctx := &structpb.Struct{
						Fields: map[string]*structpb.Value{
							"gitea_runtime_token": structpb.NewStringValue("regenerated-token"),
						},
					}
					recovered.Context = ctx
					return connect.NewResponse(&runnerv1.FetchTaskResponse{
						Task:         &recovered,
						TasksVersion: 1,
					}), nil
				}
			}

			// New request — assign pending task if available.
			if pending != nil {
				task := pending
				pending = nil
				assignments = append(assignments, assignment{task: task, key: key})
				return connect.NewResponse(&runnerv1.FetchTaskResponse{
					Task:         task,
					TasksVersion: 1,
				}), nil
			}

			return connect.NewResponse(&runnerv1.FetchTaskResponse{TasksVersion: 1}), nil
		},
	))

	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewClient(server.URL, false, "uuid", "token", time.Second, 5*time.Second)
	requestKey := gouuid.New()

	// First call: get the task.
	cleanup := client.SetRequestKey(requestKey)
	resp, err := client.FetchTask(context.Background(), connect.NewRequest(&runnerv1.FetchTaskRequest{}))
	cleanup()
	require.NoError(t, err)
	require.NotNil(t, resp.Msg.GetTask())
	assert.Equal(t, int64(99), resp.Msg.GetTask().GetId())
	assert.Equal(t, "token-99",
		resp.Msg.GetTask().GetContext().GetFields()["gitea_runtime_token"].GetStringValue())

	// Retry with same key: should get the same task back with a new token.
	cleanup = client.SetRequestKey(requestKey)
	resp2, err := client.FetchTask(context.Background(), connect.NewRequest(&runnerv1.FetchTaskRequest{}))
	cleanup()
	require.NoError(t, err)

	task2 := resp2.Msg.GetTask()
	require.NotNil(t, task2, "idempotent server should return the task on retry")
	assert.Equal(t, int64(99), task2.GetId())
	assert.Equal(t, "regenerated-token",
		task2.GetContext().GetFields()["gitea_runtime_token"].GetStringValue(),
		"token should be regenerated since the original is stored as a one-way hash")
}
