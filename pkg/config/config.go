package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Runner   RunnerConfig   `yaml:"runner"`
	Cache    CacheConfig    `yaml:"cache"`
	Snapshot SnapshotConfig `yaml:"snapshot"`
	Log      LogConfig      `yaml:"log"`
}

type ServerConfig struct {
	URL               string `yaml:"url"`
	RegistrationToken string `yaml:"registration_token"`
	Insecure          bool   `yaml:"insecure"`
}

type RunnerConfig struct {
	Name            string        `yaml:"name"`
	Labels          []string      `yaml:"labels"`
	Capacity        int           `yaml:"capacity"`
	Ephemeral       bool          `yaml:"ephemeral"`
	FetchInterval   time.Duration `yaml:"fetch_interval"`
	FetchTimeout    time.Duration `yaml:"fetch_timeout"`
	Timeout         time.Duration `yaml:"timeout"`
	GitCloneURL     string        `yaml:"git_clone_url"`
	ActionsURL      string        `yaml:"actions_url"`
	ControllerImage string        `yaml:"controller_image"`
	JobSecrets      []JobSecret   `yaml:"job_secrets"`
}

// JobSecret describes a k8s Secret to mount into job pods.
type JobSecret struct {
	Name      string `yaml:"name"`       // k8s Secret name
	MountPath string `yaml:"mount_path"` // mount as files at this path (empty = use envFrom)
}

type CacheConfig struct {
	Enabled     bool   `yaml:"enabled"`      // default: true
	Dir         string `yaml:"dir"`           // cache storage directory, default: /cache
	Port        uint16 `yaml:"port"`          // cache proxy listen port, default: 9300
	ServiceName string `yaml:"service_name"`  // k8s Service name for cache (set via CACHE_SERVICE_NAME)
	PVCName     string `yaml:"pvc_name"`      // k8s PVC name for action cache (set via CACHE_PVC_NAME)
}

// SnapshotConfig controls ZFS-backed workspace caching via VolumeSnapshot CRD.
type SnapshotConfig struct {
	Enabled       bool   `yaml:"enabled"`        // default: false
	Class         string `yaml:"class"`           // VolumeSnapshotClass name
	StorageClass  string `yaml:"storage_class"`   // StorageClass for PVCs created from snapshots
	Size          string `yaml:"size"`            // PVC size (e.g., "10Gi"), default: "10Gi"
	RetentionDays int    `yaml:"retention_days"`  // GC threshold, default: 7
}

type LogConfig struct {
	Level string `yaml:"level"`
}

// Default returns a Config with sensible defaults.
func Default() *Config {
	hostname, _ := os.Hostname()
	return &Config{
		Runner: RunnerConfig{
			Name:          hostname,
			Labels:        []string{"ubuntu-latest:docker://node:22-bookworm"},
			Capacity:      1,
			FetchInterval: 2 * time.Second,
			FetchTimeout:  5 * time.Second,
			Timeout:       3 * time.Hour,
		},
		Cache: CacheConfig{
			Enabled: true,
			Dir:     "/cache",
			Port:    9300,
		},
		Snapshot: SnapshotConfig{
			Size:          "10Gi",
			RetentionDays: 7,
		},
		Log: LogConfig{
			Level: "info",
		},
	}
}

// Load reads a YAML config file and applies defaults and env overrides.
func Load(path string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// No config file — use defaults + env overrides.
			cfg.applyEnvOverrides()
			return cfg, nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	// Re-apply defaults for zero values that weren't set in the file.
	defaults := Default()
	if cfg.Runner.Name == "" {
		cfg.Runner.Name = defaults.Runner.Name
	}
	if cfg.Runner.Capacity == 0 {
		cfg.Runner.Capacity = defaults.Runner.Capacity
	}
	if cfg.Runner.FetchInterval == 0 {
		cfg.Runner.FetchInterval = defaults.Runner.FetchInterval
	}
	if cfg.Runner.FetchTimeout == 0 {
		cfg.Runner.FetchTimeout = defaults.Runner.FetchTimeout
	}
	if cfg.Runner.Timeout == 0 {
		cfg.Runner.Timeout = defaults.Runner.Timeout
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = defaults.Log.Level
	}
	if cfg.Cache.Dir == "" {
		cfg.Cache.Dir = defaults.Cache.Dir
	}
	if cfg.Cache.Port == 0 {
		cfg.Cache.Port = defaults.Cache.Port
	}

	cfg.applyEnvOverrides()
	return cfg, nil
}

// Validate checks that required fields are set.
func (c *Config) Validate() error {
	if c.Server.URL == "" {
		return fmt.Errorf("server.url is required (set via config or SERVER_URL)")
	}
	if len(c.Runner.Labels) == 0 {
		return fmt.Errorf("runner.labels must have at least one entry")
	}
	return nil
}

func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("SERVER_URL"); v != "" {
		c.Server.URL = v
	}
	if v := os.Getenv("SERVER_REGISTRATION_TOKEN"); v != "" {
		c.Server.RegistrationToken = v
	}
	if v := os.Getenv("SERVER_INSECURE"); v != "" {
		c.Server.Insecure = v == "true" || v == "1"
	}
	if v := os.Getenv("RUNNER_NAME"); v != "" {
		c.Runner.Name = v
	}
	if v := os.Getenv("RUNNER_LABELS"); v != "" {
		c.Runner.Labels = strings.Split(v, ",")
	}
	if v := os.Getenv("RUNNER_CAPACITY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Runner.Capacity = n
		}
	}
	if v := os.Getenv("RUNNER_EPHEMERAL"); v != "" {
		c.Runner.Ephemeral = v == "true" || v == "1"
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		c.Log.Level = v
	}
	if v := os.Getenv("RUNNER_GIT_CLONE_URL"); v != "" {
		c.Runner.GitCloneURL = v
	}
	if v := os.Getenv("RUNNER_ACTIONS_URL"); v != "" {
		c.Runner.ActionsURL = v
	}
	if v := os.Getenv("CONTROLLER_IMAGE"); v != "" {
		c.Runner.ControllerImage = v
	}
	if v := os.Getenv("CACHE_ENABLED"); v != "" {
		c.Cache.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("CACHE_DIR"); v != "" {
		c.Cache.Dir = v
	}
	if v := os.Getenv("CACHE_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Cache.Port = uint16(n)
		}
	}
	if v := os.Getenv("CACHE_SERVICE_NAME"); v != "" {
		c.Cache.ServiceName = v
	}
	if v := os.Getenv("CACHE_PVC_NAME"); v != "" {
		c.Cache.PVCName = v
	}
	// Snapshot config.
	if v := os.Getenv("SNAPSHOT_ENABLED"); v != "" {
		c.Snapshot.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("SNAPSHOT_CLASS"); v != "" {
		c.Snapshot.Class = v
	}
	if v := os.Getenv("SNAPSHOT_STORAGE_CLASS"); v != "" {
		c.Snapshot.StorageClass = v
	}
	if v := os.Getenv("SNAPSHOT_SIZE"); v != "" {
		c.Snapshot.Size = v
	}
	if v := os.Getenv("SNAPSHOT_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Snapshot.RetentionDays = n
		}
	}
}
