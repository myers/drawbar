package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
	"code.forgejo.org/forgejo/actions-proto/runner/v1/runnerv1connect"
	"github.com/myers/drawbar/pkg/actions"
	forgeserver "github.com/myers/drawbar/pkg/server"
	"github.com/myers/drawbar/pkg/k8s"
	"github.com/myers/drawbar/pkg/labels"
	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// --- Mocks ---

// fakeForgejoServer creates an httptest server that handles UpdateLog and UpdateTask.
type fakeForgejoServer struct {
	mu        sync.Mutex
	logCalls  int
	taskCalls int
	lastResult runnerv1.Result
}

func (f *fakeForgejoServer) serveMux(prefix string) *http.ServeMux {
	mux := http.NewServeMux()

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

type mockExecutor struct {
	mu      sync.Mutex
	outputs []string
	idx     int
}

func (m *mockExecutor) Exec(_ context.Context, _, _, _ string, _ []string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.idx
	m.idx++
	if i < len(m.outputs) {
		return m.outputs[i], nil
	}
	return "", fmt.Errorf("container terminated")
}

type mockStreamer struct {
	content string
}

func (m *mockStreamer) StreamLogs(_ context.Context, _, _, _ string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(m.content)), nil
}

// --- Integration Test ---

func TestMakeTaskHandler_RunStep_Success(t *testing.T) {
	taskID := int64(100)
	jobName := fmt.Sprintf("server-run-%d", taskID)

	// 1. Fake Forgejo server.
	fjs := &fakeForgejoServer{}
	server := httptest.NewServer(fjs.serveMux("/api/actions"))
	t.Cleanup(server.Close)
	forgejoClient := forgeserver.NewClient(server.URL, false, "uuid", "token", time.Second, 5*time.Second)

	// 2. Fake k8s client with pre-created pod.
	k8sClient := fake.NewSimpleClientset()

	// The handler creates the job, then WatchJob looks for pods with label job-name=X.
	// We need to create the pod AFTER the job is created. Use a goroutine to watch for the job.
	go func() {
		// Wait for job to appear, then create a pod that starts Running
		// and quickly transitions to Terminated.
		for i := 0; i < 100; i++ {
			time.Sleep(10 * time.Millisecond)
			jobs, _ := k8sClient.BatchV1().Jobs("test-ns").List(context.Background(), metav1.ListOptions{})
			if len(jobs.Items) > 0 {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      jobName + "-pod",
						Namespace: "test-ns",
						Labels:    map[string]string{"job-name": jobs.Items[0].Name},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name: "runner",
								State: corev1.ContainerState{
									// Start as Terminated so getContainerResult works
									// regardless of timing. waitForContainerRunning
									// also accepts Terminated state.
									Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
								},
							},
						},
					},
				}
				k8sClient.CoreV1().Pods("test-ns").Create(context.Background(), pod, metav1.CreateOptions{})
				return
			}
		}
	}()

	// 3. Mock executor that returns state events showing step success.
	executor := &mockExecutor{
		outputs: []string{
			`{"event":"start","step":0,"name":"echo","exit_code":0,"time":"t1"}`,
			`{"event":"start","step":0,"name":"echo","exit_code":0,"time":"t1"}
{"event":"end","step":0,"name":"echo","exit_code":0,"time":"t2"}`,
		},
	}

	// 4. Mock log streamer.
	streamer := &mockStreamer{content: "hello world\n"}

	// 5. Build task with simple workflow.
	task := &runnerv1.Task{
		Id: taskID,
		WorkflowPayload: []byte(`name: Test
on: [push]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo "hello world"
`),
		Context: &structpb.Struct{
			Fields: map[string]*structpb.Value{
				"server_url": structpb.NewStringValue("https://server.example.com"),
				"token":      structpb.NewStringValue("test-token"),
			},
		},
		Secrets: map[string]string{},
	}

	// 6. Run handler.
	handler := makeTaskHandler(TaskHandlerConfig{
		K8sClient:     k8sClient,
		ServerClient: forgejoClient,
		Labels:        labels.Labels{labels.MustParse("ubuntu-latest:docker://node:24")},
		Namespace:     "test-ns",
		Timeout:       5 * time.Minute,
		WatchConfig: k8s.WatchConfig{
			PollInterval: 20 * time.Millisecond,
			Executor:     executor,
			Streamer:     streamer,
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	handler(ctx, task)

	// 7. Verify.
	// Job should have been created.
	jobs, err := k8sClient.BatchV1().Jobs("test-ns").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.NotEmpty(t, jobs.Items, "k8s job should be created")

	// Forgejo should have received log and task updates.
	fjs.mu.Lock()
	assert.Greater(t, fjs.logCalls, 0, "should have sent logs")
	assert.Greater(t, fjs.taskCalls, 0, "should have sent task updates")
	assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, fjs.lastResult)
	fjs.mu.Unlock()
}

func TestMakeTaskHandler_InvalidWorkflow(t *testing.T) {
	fjs := &fakeForgejoServer{}
	server := httptest.NewServer(fjs.serveMux("/api/actions"))
	t.Cleanup(server.Close)
	forgejoClient := forgeserver.NewClient(server.URL, false, "uuid", "token", time.Second, 5*time.Second)

	task := &runnerv1.Task{
		Id:              1,
		WorkflowPayload: []byte("not valid yaml ["),
		Context: &structpb.Struct{
			Fields: map[string]*structpb.Value{},
		},
	}

	handler := makeTaskHandler(TaskHandlerConfig{
		K8sClient:     fake.NewSimpleClientset(),
		ServerClient: forgejoClient,
		Labels:        labels.Labels{labels.MustParse("ubuntu-latest:docker://node:24")},
		Namespace:     "test-ns",
	})

	handler(context.Background(), task)

	// Should report failure.
	fjs.mu.Lock()
	assert.Equal(t, runnerv1.Result_RESULT_FAILURE, fjs.lastResult)
	fjs.mu.Unlock()
}

func TestMakeTaskHandler_NoSteps(t *testing.T) {
	fjs := &fakeForgejoServer{}
	server := httptest.NewServer(fjs.serveMux("/api/actions"))
	t.Cleanup(server.Close)
	forgejoClient := forgeserver.NewClient(server.URL, false, "uuid", "token", time.Second, 5*time.Second)

	// Workflow with only an unsupported step type → no steps after filtering.
	task := &runnerv1.Task{
		Id: 2,
		WorkflowPayload: []byte(`name: Test
on: [push]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: ./local-only-action
`),
		Context: &structpb.Struct{
			Fields: map[string]*structpb.Value{},
		},
	}

	handler := makeTaskHandler(TaskHandlerConfig{
		K8sClient:     fake.NewSimpleClientset(),
		ServerClient: forgejoClient,
		Labels:        labels.Labels{labels.MustParse("ubuntu-latest:docker://node:24")},
		Namespace:     "test-ns",
	})

	handler(context.Background(), task)

	fjs.mu.Lock()
	assert.Equal(t, runnerv1.Result_RESULT_FAILURE, fjs.lastResult)
	fjs.mu.Unlock()
}

func TestMakeTaskHandler_IfFalseStep(t *testing.T) {
	// Step with if: "false" should be included in manifest but skipped at runtime.
	env := newHandlerTestEnv(t)
	env.spawnPod("test-ns", 3)

	task := &runnerv1.Task{
		Id: 3,
		WorkflowPayload: []byte(`name: Test
on: [push]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo "skipped"
        if: "false"
`),
		Context: defaultTaskContext(),
		Secrets: map[string]string{},
	}

	handler := makeTaskHandler(TaskHandlerConfig{
		K8sClient:     env.k8sClient,
		ServerClient: env.forgejoClient,
		Labels:        labels.Labels{labels.MustParse("ubuntu-latest:docker://node:24")},
		Namespace:     "test-ns",
		Timeout:       5 * time.Minute,
		WatchConfig:   defaultWatchConfig(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	handler(ctx, task)

	// Job should be created (step included), result depends on entrypoint skipping it.
	env.assertResult(runnerv1.Result_RESULT_SUCCESS)
}

// --- Helper for handler tests ---

// handlerTestEnv sets up the common infrastructure for a makeTaskHandler test.
type handlerTestEnv struct {
	t             *testing.T
	fjs           *fakeForgejoServer
	forgejoClient *forgeserver.Client
	k8sClient     *fake.Clientset
	server        *httptest.Server
}

func newHandlerTestEnv(t *testing.T) *handlerTestEnv {
	t.Helper()
	fjs := &fakeForgejoServer{}
	server := httptest.NewServer(fjs.serveMux("/api/actions"))
	t.Cleanup(server.Close)
	return &handlerTestEnv{
		t:             t,
		fjs:           fjs,
		forgejoClient: forgeserver.NewClient(server.URL, false, "uuid", "token", time.Second, 5*time.Second),
		k8sClient:     fake.NewSimpleClientset(),
		server:        server,
	}
}

func (e *handlerTestEnv) spawnPod(ns string, taskID int64) {
	e.t.Helper()
	go func() {
		for i := 0; i < 100; i++ {
			time.Sleep(10 * time.Millisecond)
			jobs, _ := e.k8sClient.BatchV1().Jobs(ns).List(context.Background(), metav1.ListOptions{})
			if len(jobs.Items) > 0 {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: fmt.Sprintf("server-run-%d-pod", taskID), Namespace: ns,
						Labels: map[string]string{"job-name": jobs.Items[0].Name},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{{
							Name:  "runner",
							State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
						}},
					},
				}
				e.k8sClient.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{})
				return
			}
		}
	}()
}

func (e *handlerTestEnv) assertResult(expected runnerv1.Result) {
	e.t.Helper()
	e.fjs.mu.Lock()
	defer e.fjs.mu.Unlock()
	assert.Equal(e.t, expected, e.fjs.lastResult)
}

func defaultTaskContext() *structpb.Struct {
	return &structpb.Struct{
		Fields: map[string]*structpb.Value{
			"server_url": structpb.NewStringValue("https://server.example.com"),
			"token":      structpb.NewStringValue("test-token"),
			"repository": structpb.NewStringValue("myorg/myrepo"),
			"run_id":     structpb.NewStringValue("1"),
			"ref":        structpb.NewStringValue("refs/heads/main"),
			"sha":        structpb.NewStringValue("abc123"),
		},
	}
}

func defaultWatchConfig() k8s.WatchConfig {
	return k8s.WatchConfig{
		PollInterval: 20 * time.Millisecond,
		Executor:     &mockExecutor{outputs: []string{}},
		Streamer:     &mockStreamer{content: ""},
	}
}

// --- Checkout step ---

func TestMakeTaskHandler_CheckoutStep(t *testing.T) {
	env := newHandlerTestEnv(t)
	env.spawnPod("test-ns", 10)

	task := &runnerv1.Task{
		Id: 10,
		WorkflowPayload: []byte(`name: CI
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: echo "after checkout"
`),
		Context: defaultTaskContext(),
		Secrets: map[string]string{},
	}

	handler := makeTaskHandler(TaskHandlerConfig{
		K8sClient:     env.k8sClient,
		ServerClient: env.forgejoClient,
		Labels:        labels.Labels{labels.MustParse("ubuntu-latest:docker://node:24")},
		Namespace:     "test-ns",
		Timeout:       5 * time.Minute,
		WatchConfig:   defaultWatchConfig(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	handler(ctx, task)

	env.assertResult(runnerv1.Result_RESULT_SUCCESS)
}

// --- Action step (with pre-cached action) ---

func TestMakeTaskHandler_ActionStep(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	env := newHandlerTestEnv(t)
	env.spawnPod("test-ns", 20)

	cacheDir := t.TempDir()
	actionCache := actions.NewActionCache(cacheDir)

	baseDir, _, _ := setupActionRepo(t, "myorg", "my-action", map[string]string{
		"action.yml": `name: my-action
description: test
runs:
  using: node20
  main: index.js
`,
	})

	task := &runnerv1.Task{
		Id: 20,
		WorkflowPayload: []byte(`name: CI
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: myorg/my-action@main
`),
		Context: defaultTaskContext(),
		Secrets: map[string]string{},
	}

	handler := makeTaskHandler(TaskHandlerConfig{
		K8sClient:     env.k8sClient,
		ServerClient: env.forgejoClient,
		Labels:        labels.Labels{labels.MustParse("ubuntu-latest:docker://node:24")},
		Namespace:     "test-ns",
		Timeout:       5 * time.Minute,
		ActionsURL:    baseDir,
		ActionCache:   actionCache,
		CachePVCName:  "cache-pvc",
		WatchConfig:   defaultWatchConfig(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	handler(ctx, task)

	env.assertResult(runnerv1.Result_RESULT_SUCCESS)
}

// --- Services ---

func TestMakeTaskHandler_WithServices(t *testing.T) {
	env := newHandlerTestEnv(t)
	env.spawnPod("test-ns", 30)

	task := &runnerv1.Task{
		Id: 30,
		WorkflowPayload: []byte(`name: CI
on: [push]
jobs:
  test:
    runs-on: ubuntu-latest
    services:
      postgres:
        image: postgres:16
        ports:
          - 5432:5432
    steps:
      - run: echo "with services"
`),
		Context: defaultTaskContext(),
		Secrets: map[string]string{},
	}

	handler := makeTaskHandler(TaskHandlerConfig{
		K8sClient:     env.k8sClient,
		ServerClient: env.forgejoClient,
		Labels:        labels.Labels{labels.MustParse("ubuntu-latest:docker://node:24")},
		Namespace:     "test-ns",
		Timeout:       5 * time.Minute,
		WatchConfig:   defaultWatchConfig(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	handler(ctx, task)

	env.assertResult(runnerv1.Result_RESULT_SUCCESS)

	jobs, err := env.k8sClient.BatchV1().Jobs("test-ns").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, jobs.Items)
	initContainers := jobs.Items[0].Spec.Template.Spec.InitContainers
	var hasService bool
	for _, c := range initContainers {
		if strings.HasPrefix(c.Name, "svc-") {
			hasService = true
			assert.Equal(t, "postgres:16", c.Image)
		}
	}
	assert.True(t, hasService, "should have a service init container")
}

// --- Unsupported action reference ---

func TestMakeTaskHandler_UnsupportedAction(t *testing.T) {
	env := newHandlerTestEnv(t)

	task := &runnerv1.Task{
		Id: 40,
		WorkflowPayload: []byte(`name: CI
on: [push]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: ./local-action
`),
		Context: defaultTaskContext(),
		Secrets: map[string]string{},
	}

	handler := makeTaskHandler(TaskHandlerConfig{
		K8sClient:     env.k8sClient,
		ServerClient: env.forgejoClient,
		Labels:        labels.Labels{labels.MustParse("ubuntu-latest:docker://node:24")},
		Namespace:     "test-ns",
	})

	handler(context.Background(), task)

	// Local action is unsupported → no steps → failure.
	env.assertResult(runnerv1.Result_RESULT_FAILURE)
}

// --- Git helpers ---

func setupActionRepo(t *testing.T, org, repo string, files map[string]string) (string, string, string) {
	t.Helper()
	baseDir := t.TempDir()
	repoDir := filepath.Join(baseDir, org, repo+".git")
	workDir := filepath.Join(t.TempDir(), "work")
	require.NoError(t, os.MkdirAll(workDir, 0o755))
	gitRun(t, workDir, "git", "init", "-b", "main")
	gitRun(t, workDir, "git", "config", "user.email", "test@test.com")
	gitRun(t, workDir, "git", "config", "user.name", "Test")
	for name, content := range files {
		path := filepath.Join(workDir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	gitRun(t, workDir, "git", "add", ".")
	gitRun(t, workDir, "git", "commit", "-m", "init")
	require.NoError(t, os.MkdirAll(filepath.Dir(repoDir), 0o755))
	gitRun(t, "", "git", "clone", "--bare", workDir, repoDir)
	return baseDir, org, repo
}

func gitRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s %v failed:\n%s", name, args, string(out))
}
