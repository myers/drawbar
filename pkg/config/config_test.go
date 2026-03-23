package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefault(t *testing.T) {
	cfg := Default()
	assert.Equal(t, 1, cfg.Runner.Capacity)
	assert.Equal(t, 2*time.Second, cfg.Runner.FetchInterval)
	assert.Equal(t, 3*time.Hour, cfg.Runner.Timeout)
	assert.Equal(t, "info", cfg.Log.Level)
	assert.NotEmpty(t, cfg.Runner.Labels)
}

func TestLoad_NoFile(t *testing.T) {
	cfg, err := Load("/nonexistent/config.yaml")
	require.NoError(t, err)
	assert.Equal(t, Default().Runner.Capacity, cfg.Runner.Capacity)
}

func TestLoad_ValidFile(t *testing.T) {
	content := `
server:
  url: http://localhost:3000
  insecure: true
runner:
  name: test-runner
  labels:
    - "linux:docker://ubuntu:22.04"
  capacity: 4
log:
  level: debug
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:3000", cfg.Server.URL)
	assert.True(t, cfg.Server.Insecure)
	assert.Equal(t, "test-runner", cfg.Runner.Name)
	assert.Equal(t, 4, cfg.Runner.Capacity)
	assert.Equal(t, "debug", cfg.Log.Level)
	assert.Equal(t, []string{"linux:docker://ubuntu:22.04"}, cfg.Runner.Labels)
}

func TestLoad_EnvOverrides(t *testing.T) {
	t.Setenv("SERVER_URL", "http://env-override:3000")
	t.Setenv("RUNNER_NAME", "env-runner")
	t.Setenv("LOG_LEVEL", "debug")

	cfg, err := Load("/nonexistent/config.yaml")
	require.NoError(t, err)
	assert.Equal(t, "http://env-override:3000", cfg.Server.URL)
	assert.Equal(t, "env-runner", cfg.Runner.Name)
	assert.Equal(t, "debug", cfg.Log.Level)
}

func TestLoad_AllEnvOverrides(t *testing.T) {
	t.Setenv("SERVER_URL", "http://server:3000")
	t.Setenv("SERVER_REGISTRATION_TOKEN", "reg-tok")
	t.Setenv("SERVER_INSECURE", "true")
	t.Setenv("RUNNER_NAME", "my-runner")
	t.Setenv("RUNNER_LABELS", "linux:docker://ubuntu:22.04,arm:docker://arm64v8/ubuntu:22.04")
	t.Setenv("RUNNER_CAPACITY", "8")
	t.Setenv("RUNNER_GIT_CLONE_URL", "http://internal:3000")
	t.Setenv("RUNNER_ACTIONS_URL", "http://actions:3000")
	t.Setenv("CONTROLLER_IMAGE", "myregistry/runner:v1")
	t.Setenv("LOG_LEVEL", "error")
	t.Setenv("CACHE_ENABLED", "1")
	t.Setenv("CACHE_DIR", "/data/cache")
	t.Setenv("CACHE_PORT", "9400")
	t.Setenv("CACHE_SERVICE_NAME", "cache-svc")
	t.Setenv("CACHE_PVC_NAME", "cache-pvc")

	cfg, err := Load("/nonexistent/config.yaml")
	require.NoError(t, err)

	assert.Equal(t, "http://server:3000", cfg.Server.URL)
	assert.Equal(t, "reg-tok", cfg.Server.RegistrationToken)
	assert.True(t, cfg.Server.Insecure)
	assert.Equal(t, "my-runner", cfg.Runner.Name)
	assert.Equal(t, []string{"linux:docker://ubuntu:22.04", "arm:docker://arm64v8/ubuntu:22.04"}, cfg.Runner.Labels)
	assert.Equal(t, 8, cfg.Runner.Capacity)
	assert.Equal(t, "http://internal:3000", cfg.Runner.GitCloneURL)
	assert.Equal(t, "http://actions:3000", cfg.Runner.ActionsURL)
	assert.Equal(t, "myregistry/runner:v1", cfg.Runner.ControllerImage)
	assert.Equal(t, "error", cfg.Log.Level)
	assert.True(t, cfg.Cache.Enabled)
	assert.Equal(t, "/data/cache", cfg.Cache.Dir)
	assert.Equal(t, uint16(9400), cfg.Cache.Port)
	assert.Equal(t, "cache-svc", cfg.Cache.ServiceName)
	assert.Equal(t, "cache-pvc", cfg.Cache.PVCName)
}

func TestLoad_EnvOverrides_InvalidCapacity(t *testing.T) {
	t.Setenv("RUNNER_CAPACITY", "not-a-number")
	cfg, err := Load("/nonexistent/config.yaml")
	require.NoError(t, err)
	// Should keep default, not crash.
	assert.Equal(t, 1, cfg.Runner.Capacity)
}

func TestLoad_EnvOverrides_InsecureFalse(t *testing.T) {
	t.Setenv("SERVER_INSECURE", "false")
	cfg, err := Load("/nonexistent/config.yaml")
	require.NoError(t, err)
	assert.False(t, cfg.Server.Insecure)
}

func TestValidate(t *testing.T) {
	cfg := Default()
	assert.Error(t, cfg.Validate(), "should fail without URL")

	cfg.Server.URL = "http://localhost:3000"
	assert.NoError(t, cfg.Validate())

	cfg.Runner.Labels = nil
	assert.Error(t, cfg.Validate(), "should fail without labels")
}
