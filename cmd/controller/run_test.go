package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pingv1 "code.forgejo.org/forgejo/actions-proto/ping/v1"
	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
	"code.forgejo.org/forgejo/actions-proto/runner/v1/runnerv1connect"
	"github.com/myers/drawbar/pkg/config"
	forgeserver "github.com/myers/drawbar/pkg/server"
	"github.com/myers/drawbar/pkg/k8s"
	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// fullForgejoServer implements all the RPC endpoints needed for the full run() loop:
// Ping, Register, Declare, FetchTask, UpdateLog, UpdateTask.
type fullForgejoServer struct {
	mu sync.Mutex

	// FetchTask returns this task once, then blocks until context cancelled.
	task       *runnerv1.Task
	taskServed bool

	// Track reporter calls.
	logCalls   int
	taskCalls  int
	lastResult runnerv1.Result
}

func (f *fullForgejoServer) serveMux(prefix string) *http.ServeMux {
	mux := http.NewServeMux()

	mux.Handle(prefix+"/ping.v1.PingService/Ping", connect.NewUnaryHandler(
		"/ping.v1.PingService/Ping",
		func(_ context.Context, _ *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
			return connect.NewResponse(&pingv1.PingResponse{Data: "pong"}), nil
		},
	))

	mux.Handle(prefix+runnerv1connect.RunnerServiceRegisterProcedure, connect.NewUnaryHandler(
		runnerv1connect.RunnerServiceRegisterProcedure,
		func(_ context.Context, req *connect.Request[runnerv1.RegisterRequest]) (*connect.Response[runnerv1.RegisterResponse], error) {
			return connect.NewResponse(&runnerv1.RegisterResponse{
				Runner: &runnerv1.Runner{
					Id: 1, Uuid: "test-uuid", Name: req.Msg.Name, Token: "test-token",
				},
			}), nil
		},
	))

	mux.Handle(prefix+runnerv1connect.RunnerServiceDeclareProcedure, connect.NewUnaryHandler(
		runnerv1connect.RunnerServiceDeclareProcedure,
		func(_ context.Context, _ *connect.Request[runnerv1.DeclareRequest]) (*connect.Response[runnerv1.DeclareResponse], error) {
			return connect.NewResponse(&runnerv1.DeclareResponse{}), nil
		},
	))

	mux.Handle(prefix+runnerv1connect.RunnerServiceFetchTaskProcedure, connect.NewUnaryHandler(
		runnerv1connect.RunnerServiceFetchTaskProcedure,
		func(ctx context.Context, _ *connect.Request[runnerv1.FetchTaskRequest]) (*connect.Response[runnerv1.FetchTaskResponse], error) {
			f.mu.Lock()
			if !f.taskServed && f.task != nil {
				f.taskServed = true
				task := f.task
				f.mu.Unlock()
				return connect.NewResponse(&runnerv1.FetchTaskResponse{Task: task}), nil
			}
			f.mu.Unlock()

			// Block until context cancelled (simulates long-poll with no tasks).
			<-ctx.Done()
			return nil, connect.NewError(connect.CodeDeadlineExceeded, ctx.Err())
		},
	))

	mux.Handle(prefix+runnerv1connect.RunnerServiceUpdateLogProcedure, connect.NewUnaryHandler(
		runnerv1connect.RunnerServiceUpdateLogProcedure,
		func(_ context.Context, req *connect.Request[runnerv1.UpdateLogRequest]) (*connect.Response[runnerv1.UpdateLogResponse], error) {
			f.mu.Lock()
			f.logCalls++
			f.mu.Unlock()
			ack := req.Msg.Index + int64(len(req.Msg.Rows))
			return connect.NewResponse(&runnerv1.UpdateLogResponse{AckIndex: ack}), nil
		},
	))

	mux.Handle(prefix+runnerv1connect.RunnerServiceUpdateTaskProcedure, connect.NewUnaryHandler(
		runnerv1connect.RunnerServiceUpdateTaskProcedure,
		func(_ context.Context, req *connect.Request[runnerv1.UpdateTaskRequest]) (*connect.Response[runnerv1.UpdateTaskResponse], error) {
			f.mu.Lock()
			f.taskCalls++
			if req.Msg.State != nil {
				f.lastResult = req.Msg.State.Result
			}
			f.mu.Unlock()
			return connect.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
		},
	))

	return mux
}

// TestRun_FullHappyPath tests the full controller lifecycle:
// register → poll → receive task → parse workflow → create k8s job → watch → report success.
func TestRun_FullHappyPath(t *testing.T) {
	taskID := int64(42)

	// 1. Full Forgejo mock server.
	fjs := &fullForgejoServer{
		task: &runnerv1.Task{
			Id: taskID,
			WorkflowPayload: []byte(`name: CI
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo "building..."
      - run: echo "testing..."
`),
			Context: &structpb.Struct{
				Fields: map[string]*structpb.Value{
					"server_url":          structpb.NewStringValue("https://gitea.example.com"),
					"token":               structpb.NewStringValue("github-token"),
					"gitea_runtime_token": structpb.NewStringValue("runtime-token"),
					"repository":          structpb.NewStringValue("myorg/myrepo"),
					"run_id":              structpb.NewStringValue("123"),
					"event_name":          structpb.NewStringValue("push"),
					"ref":                 structpb.NewStringValue("refs/heads/main"),
				},
			},
			Secrets: map[string]string{"MY_SECRET": "super-secret"},
		},
	}
	server := httptest.NewServer(fjs.serveMux("/api/actions"))
	t.Cleanup(server.Close)

	// 2. Fake k8s client.
	k8sClient := fake.NewSimpleClientset()

	// Goroutine to create pod once job appears.
	go func() {
		for i := 0; i < 200; i++ {
			time.Sleep(10 * time.Millisecond)
			jobs, _ := k8sClient.BatchV1().Jobs("ci-jobs").List(context.Background(), metav1.ListOptions{})
			if len(jobs.Items) > 0 {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "build-pod",
						Namespace: "ci-jobs",
						Labels:    map[string]string{"job-name": jobs.Items[0].Name},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{{
							Name:  "runner",
							State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
						}},
					},
				}
				k8sClient.CoreV1().Pods("ci-jobs").Create(context.Background(), pod, metav1.CreateOptions{})
				return
			}
		}
	}()

	// 3. Mock executor returning state events for 2 steps.
	executor := &runTestExecutor{
		outputs: []string{
			`{"event":"start","step":0,"name":"echo building","exit_code":0,"time":"t1"}`,
			`{"event":"start","step":0,"name":"echo building","exit_code":0,"time":"t1"}
{"event":"end","step":0,"name":"echo building","exit_code":0,"time":"t2"}
{"event":"start","step":1,"name":"echo testing","exit_code":0,"time":"t3"}`,
			`{"event":"start","step":0,"name":"echo building","exit_code":0,"time":"t1"}
{"event":"end","step":0,"name":"echo building","exit_code":0,"time":"t2"}
{"event":"start","step":1,"name":"echo testing","exit_code":0,"time":"t3"}
{"event":"end","step":1,"name":"echo testing","exit_code":0,"time":"t4"}`,
		},
	}

	// 4. Config.
	credFile := filepath.Join(t.TempDir(), "creds.json")
	cfg := &config.Config{
		Server: config.ServerConfig{
			URL:               server.URL,
			RegistrationToken: "reg-token",
		},
		Runner: config.RunnerConfig{
			Name:          "test-runner",
			Labels:        []string{"ubuntu-latest:docker://node:24"},
			Capacity:      1,
			FetchInterval: 50 * time.Millisecond,
			FetchTimeout:  2 * time.Second,
			Timeout:       5 * time.Minute,
		},
		Log: config.LogConfig{Level: "info"},
	}

	parsedLabels, err := parseLabels(cfg.Runner.Labels)
	require.NoError(t, err)

	// 5. Run with timeout — cancel after task should be done.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	// Cancel once task is reported.
	go func() {
		for i := 0; i < 200; i++ {
			time.Sleep(25 * time.Millisecond)
			fjs.mu.Lock()
			done := fjs.lastResult != runnerv1.Result_RESULT_UNSPECIFIED
			fjs.mu.Unlock()
			if done {
				time.Sleep(50 * time.Millisecond) // let drain finish
				cancel()
				return
			}
		}
		cancel()
	}()

	err = run(ctx, cfg, runDeps{
		k8sClient: k8sClient,
		store:     &forgeserver.FileStore{Path: credFile},
		labels:    parsedLabels,
		namespace: "ci-jobs",
		logger:    slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
		watchCfg: k8s.WatchConfig{
			PollInterval: 20 * time.Millisecond,
			Executor:     executor,
			Streamer:     &runTestStreamer{content: "building output\ntesting output\n"},
		},
	})
	require.NoError(t, err)

	// 6. Verify.
	fjs.mu.Lock()
	defer fjs.mu.Unlock()
	assert.True(t, fjs.taskServed, "task should have been fetched")
	assert.Greater(t, fjs.logCalls, 0, "should have sent logs")
	assert.Greater(t, fjs.taskCalls, 0, "should have sent task state")
	assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, fjs.lastResult)

	// Job should exist in k8s.
	jobs, err := k8sClient.BatchV1().Jobs("ci-jobs").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, jobs.Items, 1)
	assert.Contains(t, jobs.Items[0].Name, fmt.Sprintf("drawbar-run-%d", taskID))

	// Credentials should have been persisted.
	store := &forgeserver.FileStore{Path: credFile}
	reg, err := store.Load(context.Background())
	require.NoError(t, err)
	require.NotNil(t, reg)
	assert.Equal(t, "test-uuid", reg.UUID)
}

// --- Test helpers ---

type runTestExecutor struct {
	mu      sync.Mutex
	outputs []string
	idx     int
}

func (m *runTestExecutor) Exec(_ context.Context, _, _, _ string, _ []string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.idx
	m.idx++
	if i < len(m.outputs) {
		return m.outputs[i], nil
	}
	return "", fmt.Errorf("container terminated")
}

type runTestStreamer struct {
	content string
}

func (m *runTestStreamer) StreamLogs(_ context.Context, _, _, _ string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(m.content)), nil
}
