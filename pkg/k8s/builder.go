package k8s

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/myers/drawbar/pkg/types"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// ServiceSpec describes a service container (sidecar) for a workflow job.
type ServiceSpec struct {
	Name  string
	Image string
	Env   map[string]string
	Ports []int32
	Cmd   []string
}

// JobSecretMount describes a k8s Secret to mount into job pods.
type JobSecretMount struct {
	Name      string // k8s Secret name
	MountPath string // mount as files at this path (empty = envFrom)
}

// JobConfig describes how to build a k8s Job.
type JobConfig struct {
	TaskID          int64
	RunID           string
	JobName         string
	Namespace       string
	Image           string // job container image (from label resolution)
	ControllerImage string // controller image (for shim injection)
	Steps           []types.StepSpec
	BaseEnv         map[string]string // env vars injected into all steps
	Services        []ServiceSpec
	Timeout          int64  // ActiveDeadlineSeconds
	CachePVCName     string // PVC for action cache (empty = skip)
	JobSecrets       []JobSecretMount // k8s Secrets to mount into job pods
	EvalContext      *types.EvalContext // evaluation context for runtime if: conditions
	WorkspacePVCName string // if set, use PVC instead of emptyDir for /workspace
}

// BuildJob creates a k8s Job with the single-container architecture.
// Services run as native sidecars. A setup-shim init container injects the
// entrypoint binary + manifest. The runner container executes all steps via
// the entrypoint.
func BuildJob(cfg JobConfig) (*batchv1.Job, error) {
	jobName := fmt.Sprintf("drawbar-run-%d", cfg.TaskID)

	// Hardened security context for all containers — drop all capabilities
	// and disallow privilege escalation. RunAsNonRoot is intentionally omitted
	// because common CI images (e.g., node:22-bookworm) run as root.
	containerSecurity := &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}

	labels := map[string]string{
		"app.kubernetes.io/managed-by": "drawbar",
		"drawbar.dev/task-id":          fmt.Sprintf("%d", cfg.TaskID),
	}
	if cfg.RunID != "" {
		labels["drawbar.dev/run-id"] = cfg.RunID
	}

	// Build the manifest JSON.
	manifest := buildManifest(cfg)
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshaling manifest: %w", err)
	}

	// Volumes.
	workspaceVol := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	if cfg.WorkspacePVCName != "" {
		workspaceVol = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: cfg.WorkspacePVCName,
			},
		}
	}
	volumes := []corev1.Volume{
		{Name: "workspace", VolumeSource: workspaceVol},
		{Name: "shim", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}

	// Action cache PVC (if any steps use actions).
	hasActions := false
	for _, step := range cfg.Steps {
		if step.ActionDir != "" {
			hasActions = true
			break
		}
	}
	if hasActions && cfg.CachePVCName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "actions-cache",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: cfg.CachePVCName,
					ReadOnly:  true,
				},
			},
		})
	}

	// Job secrets as volumes.
	for _, secret := range cfg.JobSecrets {
		if secret.MountPath != "" {
			volumes = append(volumes, corev1.Volume{
				Name: "secret-" + sanitizeK8sName(secret.Name),
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: secret.Name,
					},
				},
			})
		}
	}

	// Init containers.
	initContainers := []corev1.Container{}

	// 1. Service sidecars.
	for _, svc := range cfg.Services {
		container := corev1.Container{
			Name:            "svc-" + sanitizeK8sName(svc.Name),
			Image:           svc.Image,
			RestartPolicy:   ptr.To(corev1.ContainerRestartPolicyAlways),
			SecurityContext: containerSecurity,
		}
		for k, v := range svc.Env {
			container.Env = append(container.Env, corev1.EnvVar{Name: k, Value: v})
		}
		for _, port := range svc.Ports {
			container.Ports = append(container.Ports, corev1.ContainerPort{ContainerPort: port})
		}
		if len(svc.Cmd) > 0 {
			container.Command = svc.Cmd
		}
		initContainers = append(initContainers, container)
	}

	// 2. Wait-for-services.
	if waitScript := generateWaitScript(cfg.Services); waitScript != "" {
		initContainers = append(initContainers, corev1.Container{
			Name:            "wait-for-services",
			Image:           cfg.Image,
			Command:         []string{"/bin/sh", "-e", "-c"},
			Args:            []string{waitScript},
			SecurityContext: containerSecurity,
		})
	}

	// 3. Setup-shim: copies entrypoint binary + manifest to shared volume.
	controllerImage := cfg.ControllerImage
	if controllerImage == "" {
		controllerImage = "ghcr.io/myers/drawbar:latest"
	}

	// Generate a random heredoc delimiter to prevent injection if manifest
	// JSON happens to contain the delimiter string on a line by itself.
	delimBytes := make([]byte, 16)
	if _, err := rand.Read(delimBytes); err != nil {
		return nil, fmt.Errorf("generating heredoc delimiter: %w", err)
	}
	delimiter := "MANIFEST_" + hex.EncodeToString(delimBytes)

	setupCmd := fmt.Sprintf(
		"cp /entrypoint /shim/entrypoint && chmod +x /shim/entrypoint && "+
			"printf '#!/bin/sh\\necho \"$GIT_AUTH_TOKEN\"\\n' > /shim/askpass.sh && chmod +x /shim/askpass.sh && "+
			"cat > /shim/manifest.json << '%s'\n%s\n%s",
		delimiter, string(manifestJSON), delimiter)

	initContainers = append(initContainers, corev1.Container{
		Name:            "setup-shim",
		Image:           controllerImage,
		Command:         []string{"/bin/sh", "-c"},
		Args:            []string{setupCmd},
		SecurityContext: containerSecurity,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "shim", MountPath: "/shim"},
		},
	})

	// Runner container: executes all steps via entrypoint.
	runnerMounts := []corev1.VolumeMount{
		{Name: "workspace", MountPath: "/workspace"},
		{Name: "shim", MountPath: "/shim"},
	}

	// Add subPath mounts for each action.
	for _, step := range cfg.Steps {
		if step.ActionDir != "" && cfg.CachePVCName != "" {
			runnerMounts = append(runnerMounts, corev1.VolumeMount{
				Name:      "actions-cache",
				MountPath: "/actions/" + step.ActionDir,
				SubPath:   "actions-repo-cache/" + step.ActionDir,
				ReadOnly:  true,
			})
		}
	}

	// Add job secret mounts to runner.
	var envFrom []corev1.EnvFromSource
	for _, secret := range cfg.JobSecrets {
		if secret.MountPath != "" {
			// File mount.
			runnerMounts = append(runnerMounts, corev1.VolumeMount{
				Name:      "secret-" + sanitizeK8sName(secret.Name),
				MountPath: secret.MountPath,
				ReadOnly:  true,
			})
		} else {
			// Env vars from secret.
			envFrom = append(envFrom, corev1.EnvFromSource{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name},
				},
			})
		}
	}

	runner := corev1.Container{
		Name:            "runner",
		Image:           cfg.Image,
		Command:         []string{"/shim/entrypoint", "/shim/manifest.json"},
		WorkingDir:      "/workspace",
		VolumeMounts:    runnerMounts,
		EnvFrom:         envFrom,
		SecurityContext: containerSecurity,
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: cfg.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			Completions:             ptr.To[int32](1),
			Parallelism:             ptr.To[int32](1),
			BackoffLimit:            ptr.To[int32](0),
			TTLSecondsAfterFinished: ptr.To[int32](300),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:  corev1.RestartPolicyNever,
					InitContainers: initContainers,
					Containers:     []corev1.Container{runner},
					Volumes:        volumes,
				},
			},
		},
	}

	if cfg.Timeout > 0 {
		job.Spec.ActiveDeadlineSeconds = ptr.To[int64](cfg.Timeout)
	}

	return job, nil
}

func buildManifest(cfg JobConfig) types.Manifest {
	steps := make([]types.ManifestStep, 0, len(cfg.Steps))
	for _, s := range cfg.Steps {
		steps = append(steps, types.ManifestStep{
			ID:              s.ID,
			Name:            s.Name,
			Command:         s.Script,
			Args:            s.Args,
			Shell:           s.Shell,
			Env:             s.Env,
			WorkDir:         "/workspace",
			ContinueOnError: s.ContinueOnError,
			If:              s.If,
			TimeoutMinutes:  s.TimeoutMinutes,
		})
	}
	return types.Manifest{
		Steps:   steps,
		BaseEnv: cfg.BaseEnv,
		Context: cfg.EvalContext,
	}
}

// generateWaitScript creates a shell script that waits for service ports.
func generateWaitScript(services []ServiceSpec) string {
	var ports []int32
	for _, svc := range services {
		ports = append(ports, svc.Ports...)
	}
	if len(ports) == 0 {
		return ""
	}

	var portList []string
	for _, port := range ports {
		portList = append(portList, fmt.Sprintf("%d", port))
	}
	portsStr := strings.Join(portList, " ")

	return fmt.Sprintf(`#!/bin/sh
set -e
echo "Waiting for services on ports: %s"

check_port() {
  if command -v node >/dev/null 2>&1; then
    node -e "const s=require('net').connect($1,'localhost',()=>{s.end();process.exit(0)});s.on('error',()=>process.exit(1));setTimeout(()=>process.exit(1),1000)" 2>/dev/null
  elif command -v nc >/dev/null 2>&1; then
    nc -z localhost $1 2>/dev/null
  elif command -v bash >/dev/null 2>&1; then
    bash -c "echo > /dev/tcp/localhost/$1" 2>/dev/null
  else
    python3 -c "import socket; s=socket.socket(); s.settimeout(1); s.connect(('localhost',$1)); s.close()" 2>/dev/null
  fi
}

for i in $(seq 1 60); do
  all_ready=true
  for port in %s; do
    if ! check_port $port; then
      all_ready=false
      break
    fi
  done
  if $all_ready; then
    echo "All services ready"
    exit 0
  fi
  echo "Waiting... ($i/60)"
  sleep 1
done
echo "Services failed to start within 60s"
exit 1`, portsStr, portsStr)
}

// ParseContainerPort extracts the container port from a Docker-style port spec.
func ParseContainerPort(portSpec string) (int32, error) {
	portSpec = strings.Split(portSpec, "/")[0]
	parts := strings.Split(portSpec, ":")
	portStr := parts[len(parts)-1]
	port, err := strconv.ParseInt(portStr, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid port %q: %w", portSpec, err)
	}
	return int32(port), nil
}

func sanitizeK8sName(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteRune(c)
		} else if c == ' ' || c == '_' {
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
