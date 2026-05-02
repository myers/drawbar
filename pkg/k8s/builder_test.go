package k8s

import (
	"testing"

	"github.com/myers/drawbar/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
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
	assert.Equal(t, []string{"/shim/entrypoint", "run", "/shim/manifest.json"}, containers[0].Command)
	assert.Equal(t, "/workspace", containers[0].WorkingDir)

	// Volumes: workspace + shim + actions.
	require.Len(t, job.Spec.Template.Spec.Volumes, 3)
	assert.Equal(t, "workspace", job.Spec.Template.Spec.Volumes[0].Name)
	assert.Equal(t, "shim", job.Spec.Template.Spec.Volumes[1].Name)
	assert.Equal(t, "actions", job.Spec.Template.Spec.Volumes[2].Name)

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

func TestBuildJob_ActionsEmptyDirAndManifestActions(t *testing.T) {
	cfg := JobConfig{
		TaskID:          1,
		Namespace:       "default",
		Image:           "node:24-trixie",
		ControllerImage: "runner:latest",
		Steps: []types.StepSpec{
			{ID: "checkout", Name: "actions/checkout", Args: []string{"node", "/actions/actions-checkout-v4/dist/index.js"}, ActionDir: "actions-checkout-v4"},
		},
		Actions: []types.ActionFetch{
			{Dir: "actions-checkout-v4", URL: "http://drawbar-cache:9300/_apis/actions/actions-checkout-v4/tar"},
		},
	}

	job, err := BuildJob(cfg)
	require.NoError(t, err)

	// Pod should have an `actions` emptyDir volume and NO actions-cache PVC.
	var actionsVol *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		v := &job.Spec.Template.Spec.Volumes[i]
		if v.Name == "actions" {
			actionsVol = v
		}
		assert.NotEqual(t, "actions-cache", v.Name, "actions-cache PVC must not be present in the pod spec")
	}
	require.NotNil(t, actionsVol, "actions emptyDir volume must be present")
	require.NotNil(t, actionsVol.EmptyDir, "actions volume must be emptyDir, not PVC")

	// Setup-shim init container must mount /actions (write).
	var setupShim *corev1.Container
	for i := range job.Spec.Template.Spec.InitContainers {
		c := &job.Spec.Template.Spec.InitContainers[i]
		if c.Name == "setup-shim" {
			setupShim = c
		}
	}
	require.NotNil(t, setupShim, "setup-shim init container must exist")
	foundShimMount := false
	for _, m := range setupShim.VolumeMounts {
		if m.Name == "actions" && m.MountPath == "/actions" {
			foundShimMount = true
			assert.False(t, m.ReadOnly, "setup-shim must mount /actions read-write")
		}
	}
	assert.True(t, foundShimMount, "setup-shim must mount /actions")

	// Runner must mount /actions read-only.
	runner := job.Spec.Template.Spec.Containers[0]
	foundRunnerMount := false
	for _, m := range runner.VolumeMounts {
		if m.Name == "actions" && m.MountPath == "/actions" {
			foundRunnerMount = true
			assert.True(t, m.ReadOnly, "runner must mount /actions read-only")
		}
		assert.NotEqual(t, "actions-cache", m.Name, "runner must not have actions-cache mounts")
	}
	assert.True(t, foundRunnerMount, "runner must mount /actions")

	// Manifest JSON injected into setup-shim args must contain the Actions field.
	require.NotEmpty(t, setupShim.Args, "setup-shim must have args (the heredoc shell)")
	shellScript := setupShim.Args[0]
	assert.Contains(t, shellScript, `"actions":[`)
	assert.Contains(t, shellScript, "actions-checkout-v4")
	assert.Contains(t, shellScript, "/_apis/actions/actions-checkout-v4/tar")

	// Setup-shim must invoke `entrypoint setup`.
	assert.Contains(t, shellScript, "/shim/entrypoint setup /shim/manifest.json")
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

func TestBuildJob_ServiceSecurityOverride(t *testing.T) {
	// A service with a custom SecurityContext (e.g., BuildKit needing
	// unconfined seccomp) should use that context, while the runner
	// container retains the default hardened context.
	buildkitSC := &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(true),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
			Add:  []corev1.Capability{"SETUID", "SETGID"},
		},
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeUnconfined,
		},
	}

	cfg := JobConfig{
		TaskID:          99,
		Namespace:       "default",
		Image:           "node:24-trixie",
		ControllerImage: "runner:latest",
		Services: []ServiceSpec{
			{
				Name:            "buildkit",
				Image:           "moby/buildkit:rootless",
				Ports:           []int32{1234},
				SecurityContext: buildkitSC,
			},
		},
		Steps: []types.StepSpec{
			{ID: "build", Name: "Build", Script: "buildctl build"},
		},
	}

	job, err := BuildJob(cfg)
	require.NoError(t, err)

	initCs := job.Spec.Template.Spec.InitContainers
	// svc-buildkit + wait-for-services + setup-shim
	require.Len(t, initCs, 3)

	// Sidecar should have the custom SecurityContext with unconfined seccomp.
	sidecar := initCs[0]
	assert.Equal(t, "svc-buildkit", sidecar.Name)
	require.NotNil(t, sidecar.SecurityContext)
	require.NotNil(t, sidecar.SecurityContext.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeUnconfined, sidecar.SecurityContext.SeccompProfile.Type)

	// Runner container should NOT have unconfined seccomp (default hardened).
	runner := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "runner", runner.Name)
	assert.Nil(t, runner.SecurityContext.SeccompProfile)
}

func TestBuildJob_SnapshotCacheMountsAtSiblingPath(t *testing.T) {
	cfg := JobConfig{
		TaskID:          77,
		JobName:         "build",
		Namespace:       "drawbar",
		Image:           "node:24",
		ControllerImage: "ghcr.io/myers/drawbar:latest",
		Steps: []types.StepSpec{
			{ID: "noop", Name: "noop", Script: ":"},
		},
		SnapshotPVCName: "cache-77",
		SnapshotPaths:   []string{"target", "node_modules"},
	}

	job, err := BuildJob(cfg)
	require.NoError(t, err)

	// Volumes include snapshot-cache.
	var volNames []string
	for _, v := range job.Spec.Template.Spec.Volumes {
		volNames = append(volNames, v.Name)
	}
	assert.Contains(t, volNames, "snapshot-cache")

	// Runner has exactly one snapshot-cache mount, at /cache, with no subPath.
	runner := job.Spec.Template.Spec.Containers[0]
	var cacheMounts []corev1.VolumeMount
	for _, m := range runner.VolumeMounts {
		if m.Name == "snapshot-cache" {
			cacheMounts = append(cacheMounts, m)
		}
	}
	require.Len(t, cacheMounts, 1, "expected exactly one /cache mount, not per-path mounts")
	assert.Equal(t, "/cache", cacheMounts[0].MountPath)
	assert.Empty(t, cacheMounts[0].SubPath, "must not subPath into /workspace — that EBUSYs against actions/checkout")

	// No mounts under /workspace/<path> from the snapshot PVC.
	for _, m := range runner.VolumeMounts {
		assert.NotContains(t, m.MountPath, "/workspace/target")
		assert.NotContains(t, m.MountPath, "/workspace/node_modules")
	}
}

func TestBuildJob_NoSnapshotMountWithoutPVC(t *testing.T) {
	// Even if SnapshotPaths is set, no PVC means no mount.
	cfg := JobConfig{
		TaskID:        78,
		JobName:       "build",
		Namespace:     "drawbar",
		Image:         "node:24",
		Steps:         []types.StepSpec{{ID: "x", Name: "x", Script: ":"}},
		SnapshotPaths: []string{"target"},
	}
	job, err := BuildJob(cfg)
	require.NoError(t, err)
	for _, v := range job.Spec.Template.Spec.Volumes {
		assert.NotEqual(t, "snapshot-cache", v.Name)
	}
}

func TestBuildJob_ManifestCarriesCachePaths(t *testing.T) {
	cfg := JobConfig{
		TaskID:          79,
		JobName:         "build",
		Namespace:       "drawbar",
		Image:           "node:24",
		ControllerImage: "ghcr.io/myers/drawbar:latest",
		Steps: []types.StepSpec{
			{ID: "noop", Name: "noop", Script: ":"},
		},
		SnapshotPVCName: "cache-79",
		SnapshotPaths:   []string{"target"},
	}

	manifest := buildManifest(cfg)
	assert.Equal(t, []string{"target"}, manifest.CachePaths)
}

func TestBuildJob_NoCachePathsWithoutPVC(t *testing.T) {
	cfg := JobConfig{
		TaskID:        80,
		JobName:       "build",
		Namespace:     "drawbar",
		Image:         "node:24",
		Steps:         []types.StepSpec{{ID: "x", Name: "x", Script: ":"}},
		SnapshotPaths: []string{"target"},
	}
	manifest := buildManifest(cfg)
	assert.Empty(t, manifest.CachePaths)
}

func TestBuildJob_PodHasHostUsersFalse(t *testing.T) {
	job, err := BuildJob(JobConfig{
		TaskID:    1,
		JobName:   "j",
		Namespace: "gitea",
		Image:     "node:24-trixie",
	})
	require.NoError(t, err)
	require.NotNil(t, job.Spec.Template.Spec.HostUsers, "PodSpec.HostUsers must be set explicitly")
	assert.False(t, *job.Spec.Template.Spec.HostUsers, "drawbar job pods must run with userns (HostUsers: false)")
}

func TestBuildJob_RunnerContainerDoesNotPinUID(t *testing.T) {
	// We rely on the image's USER directive (typically root) and on userns
	// to make in-container root harmless on the host. Pinning RunAsUser or
	// RunAsNonRoot would break apt-get etc. inside common CI images.
	job, err := BuildJob(JobConfig{
		TaskID:    1,
		JobName:   "j",
		Namespace: "gitea",
		Image:     "node:24-trixie",
	})
	require.NoError(t, err)
	var runner *corev1.Container
	for i := range job.Spec.Template.Spec.Containers {
		if job.Spec.Template.Spec.Containers[i].Name == "runner" {
			runner = &job.Spec.Template.Spec.Containers[i]
		}
	}
	require.NotNil(t, runner)
	require.NotNil(t, runner.SecurityContext)
	assert.Nil(t, runner.SecurityContext.RunAsUser, "runner container must not pin RunAsUser")
	assert.Nil(t, runner.SecurityContext.RunAsGroup, "runner container must not pin RunAsGroup")
	assert.Nil(t, runner.SecurityContext.RunAsNonRoot, "runner container must not set RunAsNonRoot")
}
