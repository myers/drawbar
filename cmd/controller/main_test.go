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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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

// --- buildGitHubEnv ---

func TestBuildGitHubEnv(t *testing.T) {
	taskCtx := map[string]*structpb.Value{
		"server_url":       structpb.NewStringValue("https://gitea.example.com"),
		"repository":       structpb.NewStringValue("myorg/myrepo"),
		"repository_owner": structpb.NewStringValue("myorg"),
		"run_id":           structpb.NewStringValue("42"),
		"sha":              structpb.NewStringValue("abc123"),
		"ref":              structpb.NewStringValue("refs/heads/main"),
		"event_name":       structpb.NewStringValue("push"),
		"token":            structpb.NewStringValue("secret-token"),
	}

	env := make(map[string]string)
	buildGitHubEnv(env, taskCtx)

	assert.Equal(t, "https://gitea.example.com", env["GITHUB_SERVER_URL"])
	assert.Equal(t, "myorg/myrepo", env["GITHUB_REPOSITORY"])
	assert.Equal(t, "myorg", env["GITHUB_REPOSITORY_OWNER"])
	assert.Equal(t, "42", env["GITHUB_RUN_ID"])
	assert.Equal(t, "abc123", env["GITHUB_SHA"])
	assert.Equal(t, "refs/heads/main", env["GITHUB_REF"])
	assert.Equal(t, "push", env["GITHUB_EVENT_NAME"])
	assert.Equal(t, "secret-token", env["GITHUB_TOKEN"])
}

func TestBuildGitHubEnv_OIDC(t *testing.T) {
	taskCtx := map[string]*structpb.Value{
		"server_url": structpb.NewStringValue("https://gitea.example.com"),
		"forgejo_actions_id_token_request_token": structpb.NewStringValue("oidc-jwt-token"),
		"forgejo_actions_id_token_request_url":   structpb.NewStringValue("https://gitea.example.com/api/actions/_apis/pipelines/workflows/1/idtoken"),
	}

	env := make(map[string]string)
	buildGitHubEnv(env, taskCtx)

	assert.Equal(t, "oidc-jwt-token", env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"])
	assert.Equal(t, "https://gitea.example.com/api/actions/_apis/pipelines/workflows/1/idtoken", env["ACTIONS_ID_TOKEN_REQUEST_URL"])
}

func TestBuildGitHubEnv_OIDC_NotPresent(t *testing.T) {
	// When Gitea doesn't inject OIDC fields (disabled or fork PR), env vars should be absent.
	taskCtx := map[string]*structpb.Value{
		"server_url": structpb.NewStringValue("https://gitea.example.com"),
		"token":      structpb.NewStringValue("test-token"),
	}

	env := make(map[string]string)
	buildGitHubEnv(env, taskCtx)

	_, hasToken := env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"]
	_, hasURL := env["ACTIONS_ID_TOKEN_REQUEST_URL"]
	assert.False(t, hasToken, "OIDC token should not be set when not in context")
	assert.False(t, hasURL, "OIDC URL should not be set when not in context")
}

func TestBuildGitHubEnv_EmptyContext(t *testing.T) {
	env := make(map[string]string)
	buildGitHubEnv(env, map[string]*structpb.Value{})
	// No keys should be set for missing context values.
	assert.Empty(t, env)
}

// --- BuildKit auto-detection ---

func TestIsBuildKitImage(t *testing.T) {
	assert.True(t, isBuildKitImage("moby/buildkit:rootless"))
	assert.True(t, isBuildKitImage("moby/buildkit:latest"))
	assert.True(t, isBuildKitImage("docker.io/moby/buildkit:v0.20"))
	assert.False(t, isBuildKitImage("postgres:16"))
	assert.False(t, isBuildKitImage("alpine"))
}

func TestConvertServices_BuildKitAutoDetect(t *testing.T) {
	services := map[string]*model.ContainerSpec{
		"buildkit": {
			Image: "moby/buildkit:rootless",
			Ports: []string{"1234"},
		},
	}

	result := convertServices(services)
	require.Len(t, result, 1)
	svc := result[0]

	// Should have custom SecurityContext with unconfined seccomp.
	require.NotNil(t, svc.SecurityContext)
	require.NotNil(t, svc.SecurityContext.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeUnconfined, svc.SecurityContext.SeccompProfile.Type)

	// Should have SETUID+SETGID caps added.
	assert.Contains(t, svc.SecurityContext.Capabilities.Add, corev1.Capability("SETUID"))
	assert.Contains(t, svc.SecurityContext.Capabilities.Add, corev1.Capability("SETGID"))

	// Should inject --oci-worker-no-process-sandbox and --addr as Args.
	assert.Contains(t, svc.Args, "--oci-worker-no-process-sandbox")
	assert.Contains(t, svc.Args, "--addr=tcp://0.0.0.0:1234")

	// Cmd should remain empty (don't override image entrypoint).
	assert.Empty(t, svc.Cmd)
}

func TestConvertServices_BuildKitNoDoubleInject(t *testing.T) {
	services := map[string]*model.ContainerSpec{
		"buildkit": {
			Image: "moby/buildkit:rootless",
			Ports: []string{"1234"},
			Cmd:   []string{"--oci-worker-no-process-sandbox"},
		},
	}

	result := convertServices(services)
	require.Len(t, result, 1)

	// The Cmd is set by the user (override entrypoint); Args should
	// still get the flag since user set it in Cmd, not Args.
	// But the auto-detect checks Args, so it will add it.
	count := 0
	for _, arg := range result[0].Args {
		if arg == "--oci-worker-no-process-sandbox" {
			count++
		}
	}
	assert.Equal(t, 1, count, "should not double-inject in Args")
}
