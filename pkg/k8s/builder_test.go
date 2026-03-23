package k8s

import (
	"testing"

	"github.com/myers/drawbar/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestBuildJob_SingleContainer(t *testing.T) {
	cfg := JobConfig{
		TaskID:          42,
		RunID:           "7",
		JobName:         "build",
		Namespace:       "drawbar",
		Image:           "node:24-trixie",
		ControllerImage: "ghcr.io/myers/drawbar:latest",
		Steps: []types.StepSpec{
			{ID: "greet", Name: "Greet", Script: `echo "hello world"`},
			{ID: "build", Name: "Build", Shell: "bash", Script: "make build"},
		},
		BaseEnv: map[string]string{"CI": "true"},
	}

	job, err := BuildJob(cfg)
	require.NoError(t, err)

	assert.Equal(t, "drawbar-run-42", job.Name)
	assert.Equal(t, "drawbar", job.Namespace)
	assert.Equal(t, "drawbar", job.Labels["app.kubernetes.io/managed-by"])

	// Init containers: just setup-shim (no step init containers anymore).
	initCs := job.Spec.Template.Spec.InitContainers
	require.Len(t, initCs, 1)
	assert.Equal(t, "setup-shim", initCs[0].Name)
	assert.Equal(t, "ghcr.io/myers/drawbar:latest", initCs[0].Image)

	// Main container: runner.
	containers := job.Spec.Template.Spec.Containers
	require.Len(t, containers, 1)
	assert.Equal(t, "runner", containers[0].Name)
	assert.Equal(t, "node:24-trixie", containers[0].Image)
	assert.Equal(t, []string{"/shim/entrypoint", "/shim/manifest.json"}, containers[0].Command)
	assert.Equal(t, "/workspace", containers[0].WorkingDir)

	// Volumes: workspace + shim.
	require.Len(t, job.Spec.Template.Spec.Volumes, 2)
	assert.Equal(t, "workspace", job.Spec.Template.Spec.Volumes[0].Name)
	assert.Equal(t, "shim", job.Spec.Template.Spec.Volumes[1].Name)

	// Job config.
	assert.Equal(t, int32(0), *job.Spec.BackoffLimit)
}

func TestBuildJob_WithServices(t *testing.T) {
	cfg := JobConfig{
		TaskID:          1,
		Namespace:       "default",
		Image:           "node:24-trixie",
		ControllerImage: "runner:latest",
		Services: []ServiceSpec{
			{Name: "postgres", Image: "postgres:16", Ports: []int32{5432}},
		},
		Steps: []types.StepSpec{
			{ID: "test", Name: "Test", Script: "echo test"},
		},
	}

	job, err := BuildJob(cfg)
	require.NoError(t, err)

	initCs := job.Spec.Template.Spec.InitContainers
	// svc-postgres (sidecar) + wait-for-services + setup-shim
	require.Len(t, initCs, 3)

	assert.Equal(t, "svc-postgres", initCs[0].Name)
	require.NotNil(t, initCs[0].RestartPolicy)
	assert.Equal(t, corev1.ContainerRestartPolicyAlways, *initCs[0].RestartPolicy)

	assert.Equal(t, "wait-for-services", initCs[1].Name)
	assert.Equal(t, "setup-shim", initCs[2].Name)
}

func TestBuildJob_WithActions(t *testing.T) {
	cfg := JobConfig{
		TaskID:          1,
		Namespace:       "default",
		Image:           "node:24-trixie",
		ControllerImage: "runner:latest",
		CachePVCName:    "runner-cache",
		Steps: []types.StepSpec{
			{ID: "cache", Name: "actions/cache", Script: "node /actions/ac/dist/index.js", ActionDir: "ac"},
		},
	}

	job, err := BuildJob(cfg)
	require.NoError(t, err)

	// Should have actions-cache PVC volume.
	foundPVC := false
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "actions-cache" {
			foundPVC = true
			assert.Equal(t, "runner-cache", v.PersistentVolumeClaim.ClaimName)
			assert.True(t, v.PersistentVolumeClaim.ReadOnly)
		}
	}
	assert.True(t, foundPVC, "actions-cache PVC should be present")

	// Runner should have subPath mount.
	runner := job.Spec.Template.Spec.Containers[0]
	foundMount := false
	for _, m := range runner.VolumeMounts {
		if m.Name == "actions-cache" {
			foundMount = true
			assert.Equal(t, "/actions/ac", m.MountPath)
			assert.Equal(t, "actions-repo-cache/ac", m.SubPath)
			assert.True(t, m.ReadOnly)
		}
	}
	assert.True(t, foundMount, "action subPath mount should be present")
}

func TestBuildJob_ManifestInSetupShim(t *testing.T) {
	cfg := JobConfig{
		TaskID:          1,
		Namespace:       "default",
		Image:           "node:24-trixie",
		ControllerImage: "runner:latest",
		Steps: []types.StepSpec{
			{ID: "hello", Name: "Hello", Script: "echo hi", Shell: "sh"},
		},
		BaseEnv: map[string]string{"FOO": "bar"},
	}

	job, err := BuildJob(cfg)
	require.NoError(t, err)

	// The setup-shim init container should contain the manifest in its args.
	setupShim := job.Spec.Template.Spec.InitContainers[0]
	assert.Equal(t, "setup-shim", setupShim.Name)
	require.Len(t, setupShim.Args, 1)
	assert.Contains(t, setupShim.Args[0], `"id":"hello"`)
	assert.Contains(t, setupShim.Args[0], `"FOO":"bar"`)
}

func TestBuildJob_Timeout(t *testing.T) {
	cfg := JobConfig{
		TaskID:          1,
		Namespace:       "default",
		Image:           "alpine",
		ControllerImage: "runner:latest",
		Steps:           []types.StepSpec{{ID: "x", Script: "true"}},
		Timeout:         3600,
	}

	job, err := BuildJob(cfg)
	require.NoError(t, err)
	require.NotNil(t, job.Spec.ActiveDeadlineSeconds)
	assert.Equal(t, int64(3600), *job.Spec.ActiveDeadlineSeconds)
}

func TestParseContainerPort(t *testing.T) {
	tests := []struct {
		input    string
		expected int32
		wantErr  bool
	}{
		{"5432", 5432, false},
		{"5432:5432", 5432, false},
		{"8080:80", 80, false},
		{"5432/tcp", 5432, false},
		{"abc", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			port, err := ParseContainerPort(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, port)
			}
		})
	}
}

func TestGenerateWaitScript(t *testing.T) {
	assert.Equal(t, "", generateWaitScript(nil))
	script := generateWaitScript([]ServiceSpec{
		{Name: "pg", Ports: []int32{5432}},
	})
	assert.Contains(t, script, "5432")
}
