package main

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
	"github.com/nektos/act/pkg/model"
	"github.com/myers/drawbar/pkg/config"
	"github.com/myers/drawbar/pkg/server"
	"github.com/myers/drawbar/pkg/labels"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"short", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"long", "hello world", 5, "hello..."},
		{"empty", "", 5, ""},
		{"unicode", "こんにちは世界", 3, "こんに..."},
		{"zero", "hello", 0, "..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.s, tt.n)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCleanupOrphanedJobs(t *testing.T) {
	ctx := context.Background()
	ns := "test-ns"

	activeJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "active-job",
			Namespace: ns,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "drawbar"},
		},
		Status: batchv1.JobStatus{Active: 1},
	}
	completedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "completed-job",
			Namespace: ns,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "drawbar"},
		},
		Status: batchv1.JobStatus{Active: 0},
	}

	client := fake.NewSimpleClientset(activeJob, completedJob)
	cleanupOrphanedJobs(ctx, client, ns)

	// Active job should be deleted.
	jobs, err := client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)

	var remaining []string
	for _, j := range jobs.Items {
		remaining = append(remaining, j.Name)
	}
	assert.Contains(t, remaining, "completed-job")
	assert.NotContains(t, remaining, "active-job")
}

func TestCleanupOrphanedJobs_NoJobs(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	// Should not panic or error.
	cleanupOrphanedJobs(ctx, client, "empty-ns")
}

// --- convertServices ---

func TestConvertServices(t *testing.T) {
	services := map[string]*model.ContainerSpec{
		"postgres": {
			Image: "postgres:16",
			Env:   map[string]string{"POSTGRES_PASSWORD": "test"},
			Ports: []string{"5432"},
			Cmd:   []string{"postgres", "-c", "log_statement=all"},
		},
		"redis": {
			Image: "redis:7",
			Ports: []string{"6379"},
		},
		"empty": nil,
		"no-image": {
			Image: "",
		},
	}

	result := convertServices(services)

	// nil and empty image should be skipped.
	assert.Len(t, result, 2)

	names := map[string]bool{}
	for _, svc := range result {
		names[svc.Name] = true
	}
	assert.True(t, names["postgres"])
	assert.True(t, names["redis"])
}

func TestConvertServices_InvalidPort(t *testing.T) {
	services := map[string]*model.ContainerSpec{
		"svc": {
			Image: "alpine",
			Ports: []string{"not-a-port", "8080"},
		},
	}
	result := convertServices(services)
	require.Len(t, result, 1)
	// Only valid port should be included.
	assert.Len(t, result[0].Ports, 1)
	assert.Equal(t, int32(8080), result[0].Ports[0])
}

// --- buildArtifactEnv ---

func TestBuildArtifactEnv(t *testing.T) {
	t.Run("uses server URL", func(t *testing.T) {
		env := make(map[string]string)
		buildArtifactEnv(env, "https://gitea.example.com/", "")
		assert.Equal(t, "https://gitea.example.com/api/actions_pipeline/", env["ACTIONS_RUNTIME_URL"])
		assert.Equal(t, "https://gitea.example.com/", env["ACTIONS_RESULTS_URL"])
	})
	t.Run("gitCloneURL overrides", func(t *testing.T) {
		env := make(map[string]string)
		buildArtifactEnv(env, "https://public.example.com", "https://internal.example.com")
		assert.Equal(t, "https://internal.example.com/api/actions_pipeline/", env["ACTIONS_RUNTIME_URL"])
	})
}

// --- convertJobSecrets ---

func TestConvertJobSecrets(t *testing.T) {
	secrets := []config.JobSecret{
		{Name: "my-secret", MountPath: "/secrets/my-secret"},
		{Name: "env-secret", MountPath: ""},
	}
	mounts := convertJobSecrets(secrets)
	require.Len(t, mounts, 2)
	assert.Equal(t, "my-secret", mounts[0].Name)
	assert.Equal(t, "/secrets/my-secret", mounts[0].MountPath)
	assert.Equal(t, "", mounts[1].MountPath)
}

func TestConvertJobSecrets_Empty(t *testing.T) {
	mounts := convertJobSecrets(nil)
	assert.Nil(t, mounts)
}

// --- resolveJobImage ---

func TestResolveJobImage_FromLabels(t *testing.T) {
	l := labels.Labels{labels.MustParse("ubuntu-latest:docker://node:24")}
	image := resolveJobImage(l, []string{"ubuntu-latest"}, nil)
	assert.Equal(t, "node:24", image)
}

func TestResolveJobImage_ContainerOverride(t *testing.T) {
	l := labels.Labels{labels.MustParse("ubuntu-latest:docker://node:24")}
	container := &model.ContainerSpec{Image: "custom:latest"}
	image := resolveJobImage(l, []string{"ubuntu-latest"}, container)
	assert.Equal(t, "custom:latest", image)
}

func TestResolveJobImage_NilContainer(t *testing.T) {
	l := labels.Labels{labels.MustParse("ubuntu-latest:docker://node:24")}
	image := resolveJobImage(l, []string{"ubuntu-latest"}, nil)
	assert.Equal(t, "node:24", image)
}

func TestResolveJobImage_EmptyContainerImage(t *testing.T) {
	l := labels.Labels{labels.MustParse("ubuntu-latest:docker://node:24")}
	container := &model.ContainerSpec{Image: ""}
	image := resolveJobImage(l, []string{"ubuntu-latest"}, container)
	assert.Equal(t, "node:24", image)
}

// --- collectSecrets ---

// --- parseTimeoutMinutes ---

func TestParseTimeoutMinutes(t *testing.T) {
	tests := []struct {
		input string
		want  float64
	}{
		{"", 0},
		{"5", 5},
		{"1.5", 1.5},
		{"0", 0},
		{"-1", 0},
		{"invalid", 0},
		{"120", 120},
		{"0.1", 0.1},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseTimeoutMinutes(tt.input)
			assert.InDelta(t, tt.want, got, 0.001)
		})
	}
}

func TestCollectSecrets(t *testing.T) {
	task := &runnerv1.Task{
		Secrets: map[string]string{
			"MY_SECRET": "secret-value",
		},
		Context: &structpb.Struct{
			Fields: map[string]*structpb.Value{
				"token":                structpb.NewStringValue("github-token"),
				"gitea_runtime_token":  structpb.NewStringValue("runtime-token"),
			},
		},
	}

	secrets := collectSecrets(task)
	assert.Contains(t, secrets, "secret-value")
	assert.Contains(t, secrets, "github-token")
	assert.Contains(t, secrets, "runtime-token")
}

// --- healthzHandler ---

func TestHealthzHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	healthzHandler(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ok", w.Body.String())
}

// --- readyzHandler ---

func TestReadyzHandler_Ready(t *testing.T) {
	var registered atomic.Bool
	registered.Store(true)
	handler := readyzHandler(&registered)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestReadyzHandler_NotReady(t *testing.T) {
	var registered atomic.Bool
	registered.Store(false)
	handler := readyzHandler(&registered)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// --- newLogrusLogger ---

func TestNewLogrusLogger(t *testing.T) {
	l := newLogrusLogger()
	assert.NotNil(t, l)
	_, ok := l.Formatter.(*logrus.JSONFormatter)
	assert.True(t, ok, "expected JSON formatter")
}

// --- startCacheServer ---

func TestStartCacheServer(t *testing.T) {
	cacheDir := t.TempDir()
	cfg := config.CacheConfig{
		Enabled: true,
		Dir:     cacheDir,
		Port:    0,
	}

	handler, err := startCacheServer(cfg)
	require.NoError(t, err)
	require.NotNil(t, handler)
	defer handler.Close()

	assert.NotEmpty(t, handler.ExternalURL())
	_, err = os.Stat(cacheDir)
	assert.NoError(t, err)
}

// --- setupLogging / parseLabels ---

func TestSetupLogging(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error", ""} {
		logger := setupLogging(level)
		assert.NotNil(t, logger)
	}
}

func TestParseLabels(t *testing.T) {
	labels, err := parseLabels([]string{"ubuntu:docker://node:24", "self:host"})
	require.NoError(t, err)
	assert.Len(t, labels, 2)
}

func TestParseLabels_Invalid(t *testing.T) {
	_, err := parseLabels([]string{"bad:invalidscheme"})
	assert.Error(t, err)
}

// --- parseFlags ---

func TestParseFlags_Defaults(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	f, err := parseFlags(fs, []string{})
	require.NoError(t, err)
	assert.Equal(t, "config.yaml", f.ConfigPath)
	assert.Equal(t, "", f.CredentialFile)
	assert.Equal(t, "drawbar", f.SecretName)
	assert.Equal(t, "", f.SecretNamespace)
	assert.Equal(t, "", f.Kubeconfig)
	assert.Equal(t, "", f.JobNamespace)
}

func TestParseFlags_AllSet(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	f, err := parseFlags(fs, []string{
		"-config", "/etc/runner.yaml",
		"-credential-file", "/creds.json",
		"-secret-name", "my-secret",
		"-secret-namespace", "runner-ns",
		"-kubeconfig", "/home/user/.kube/config",
		"-job-namespace", "jobs",
	})
	require.NoError(t, err)
	assert.Equal(t, "/etc/runner.yaml", f.ConfigPath)
	assert.Equal(t, "/creds.json", f.CredentialFile)
	assert.Equal(t, "my-secret", f.SecretName)
	assert.Equal(t, "runner-ns", f.SecretNamespace)
	assert.Equal(t, "/home/user/.kube/config", f.Kubeconfig)
	assert.Equal(t, "jobs", f.JobNamespace)
}

func TestParseFlags_UnknownFlag(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	_, err := parseFlags(fs, []string{"-unknown-flag"})
	assert.Error(t, err)
}

// --- resolveNamespace ---

func TestResolveNamespace(t *testing.T) {
	assert.Equal(t, "explicit", resolveNamespace("explicit", "fallback"))
	assert.Equal(t, "fallback", resolveNamespace("", "fallback"))
}

// --- createStore ---

func TestCreateStore_FileStore(t *testing.T) {
	store := createStore("/path/to/creds.json", nil, "ns", "secret")
	_, ok := store.(*server.FileStore)
	assert.True(t, ok)
}

func TestCreateStore_SecretStore(t *testing.T) {
	k8sClient := fake.NewSimpleClientset()
	store := createStore("", k8sClient, "runner-ns", "runner-secret")
	ss, ok := store.(*server.SecretStore)
	assert.True(t, ok)
	assert.Equal(t, "runner-ns", ss.Namespace)
	assert.Equal(t, "runner-secret", ss.Name)
}

func TestCollectSecrets_NoContext(t *testing.T) {
	task := &runnerv1.Task{
		Secrets: map[string]string{"A": "val-a"},
	}
	secrets := collectSecrets(task)
	assert.Contains(t, secrets, "val-a")
}
