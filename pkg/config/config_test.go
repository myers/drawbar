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
	t.Setenv("RUNNER_NAME", "")
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

func TestValidate_Capacity(t *testing.T) {
	cfg := validConfig()
	cfg.Runner.Capacity = 0
	assert.ErrorContains(t, cfg.Validate(), "capacity")
}

func TestValidate_FetchInterval(t *testing.T) {
	cfg := validConfig()
	cfg.Runner.FetchInterval = 10 * time.Millisecond
	assert.ErrorContains(t, cfg.Validate(), "fetch_interval")
}

func TestValidate_FetchTimeout(t *testing.T) {
	cfg := validConfig()
	cfg.Runner.FetchTimeout = 1 * time.Millisecond // less than fetch_interval
	assert.ErrorContains(t, cfg.Validate(), "fetch_timeout")
}

func TestValidate_Timeout(t *testing.T) {
	cfg := validConfig()
	cfg.Runner.Timeout = 30 * time.Second
	assert.ErrorContains(t, cfg.Validate(), "timeout")
}

func TestValidate_CacheDir(t *testing.T) {
	cfg := validConfig()
	cfg.Cache.Enabled = true
	cfg.Cache.Dir = ""
	assert.ErrorContains(t, cfg.Validate(), "cache.dir")
}

func TestValidate_SnapshotClass(t *testing.T) {
	cfg := validConfig()
	cfg.Snapshot.Enabled = true
	cfg.Snapshot.Class = ""
	assert.ErrorContains(t, cfg.Validate(), "snapshot.class")
}

func TestValidate_SnapshotStorageClass(t *testing.T) {
	cfg := validConfig()
	cfg.Snapshot.Enabled = true
	cfg.Snapshot.Class = "zfs"
	cfg.Snapshot.StorageClass = ""
	assert.ErrorContains(t, cfg.Validate(), "snapshot.storage_class")
}

func TestValidate_SnapshotRetention(t *testing.T) {
	cfg := validConfig()
	cfg.Snapshot.Enabled = true
	cfg.Snapshot.Class = "zfs"
	cfg.Snapshot.StorageClass = "openebs-zfs"
	cfg.Snapshot.RetentionDays = 0
	assert.ErrorContains(t, cfg.Validate(), "retention_days")
}

func TestValidate_LogLevel(t *testing.T) {
	cfg := validConfig()
	cfg.Log.Level = "trace"
	assert.ErrorContains(t, cfg.Validate(), "log.level")

	for _, level := range []string{"debug", "info", "warn", "error"} {
		cfg.Log.Level = level
		assert.NoError(t, cfg.Validate(), "level %q should be valid", level)
	}
}

func validConfig() *Config {
	cfg := Default()
	cfg.Server.URL = "http://localhost:3000"
	return cfg
}

func TestConfig_AptProxyURL_FromEnv(t *testing.T) {
	t.Setenv("RUNNER_APT_PROXY_URL", "http://apt-cache.gitea.svc:3142")
	t.Setenv("SERVER_URL", "https://example")
	cfg, err := Load("/nonexistent")
	require.NoError(t, err)
	assert.Equal(t, "http://apt-cache.gitea.svc:3142", cfg.Runner.AptProxyURL)
}

func TestConfig_AptProxyURL_FromYAML(t *testing.T) {
	tmp, err := os.CreateTemp("", "drawbar-cfg-*.yaml")
	require.NoError(t, err)
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(`
server:
  url: https://example
runner:
  apt_proxy_url: http://apt-cache.gitea.svc:3142
  labels: ["x:docker://alpine"]
`)
	require.NoError(t, err)
	tmp.Close()

	cfg, err := Load(tmp.Name())
	require.NoError(t, err)
	assert.Equal(t, "http://apt-cache.gitea.svc:3142", cfg.Runner.AptProxyURL)
}

func TestDefault_ShutdownTimeout(t *testing.T) {
	cfg := Default()
	// With default Runner.Timeout=3h and FetchTimeout=30s,
	// min(3h, 10*30s)=5m, clamped to [30s, 5m] = 5m.
	assert.Equal(t, 5*time.Minute, cfg.Runner.ShutdownTimeout)
}

func TestLoad_ShutdownTimeout_FromYAML(t *testing.T) {
	t.Setenv("RUNNER_NAME", "")
	content := `
server:
  url: http://localhost:3000
runner:
  labels: ["x:docker://alpine"]
  shutdown_timeout: 90s
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, cfg.Runner.ShutdownTimeout)
}

func TestLoad_ShutdownTimeout_DefaultAppliedForZero(t *testing.T) {
	// YAML omits shutdown_timeout entirely — Load should fall back to Default.
	t.Setenv("RUNNER_NAME", "")
	content := `
server:
  url: http://localhost:3000
runner:
  labels: ["x:docker://alpine"]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Minute, cfg.Runner.ShutdownTimeout)
}

func TestLoad_ShutdownTimeout_FromEnv(t *testing.T) {
	t.Setenv("SERVER_URL", "http://server:3000")
	t.Setenv("RUNNER_SHUTDOWN_TIMEOUT", "2m")
	cfg, err := Load("/nonexistent/config.yaml")
	require.NoError(t, err)
	assert.Equal(t, 2*time.Minute, cfg.Runner.ShutdownTimeout)
}

func TestValidate_ShutdownTimeout_TooSmall(t *testing.T) {
	cfg := validConfig()
	cfg.Runner.ShutdownTimeout = 500 * time.Millisecond
	assert.ErrorContains(t, cfg.Validate(), "shutdown_timeout")
}

func TestValidate_ShutdownTimeout_LargerThanTimeout(t *testing.T) {
	cfg := validConfig()
	cfg.Runner.Timeout = 2 * time.Minute
	cfg.Runner.ShutdownTimeout = 5 * time.Minute
	assert.ErrorContains(t, cfg.Validate(), "shutdown_timeout")
}

func TestDefault_ShutdownTimeout_ClampLow(t *testing.T) {
	// Construct a config where 10*FetchTimeout would be below 30s and verify
	// the clamp floor kicks in. Since Default() uses the global defaults, we
	// exercise the helper directly rather than via Default().
	got := computeShutdownTimeoutDefault(1*time.Hour, 1*time.Second)
	assert.Equal(t, 30*time.Second, got)
}

func TestDefault_ShutdownTimeout_ClampHigh(t *testing.T) {
	got := computeShutdownTimeoutDefault(3*time.Hour, 10*time.Minute)
	assert.Equal(t, 5*time.Minute, got)
}

func TestDefault_ShutdownTimeout_PicksMin(t *testing.T) {
	// Runner.Timeout is the lower bound — short jobs get short drain.
	got := computeShutdownTimeoutDefault(45*time.Second, 30*time.Second)
	// min(45s, 10*30s) = 45s, clamped to [30s, 5m] = 45s.
	assert.Equal(t, 45*time.Second, got)
}
