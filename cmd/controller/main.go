package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
	"code.forgejo.org/forgejo/runner/v12/act/artifactcache"
	"code.forgejo.org/forgejo/runner/v12/act/cacheproxy"
	"code.forgejo.org/forgejo/runner/v12/act/exprparser"

	"code.forgejo.org/forgejo/runner/v12/act/model"

	snapshotclient "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned"

	"github.com/myers/drawbar/pkg/actions"
	"github.com/myers/drawbar/pkg/config"
	"github.com/myers/drawbar/pkg/expressions"
	"github.com/myers/drawbar/pkg/server"
	"github.com/myers/drawbar/pkg/k8s"
	"github.com/myers/drawbar/pkg/labels"
	"github.com/myers/drawbar/pkg/reporter"
	"github.com/myers/drawbar/pkg/snapshot"
	"github.com/myers/drawbar/pkg/types"

	"github.com/sirupsen/logrus"
	"github.com/myers/drawbar/pkg/version"
	"github.com/myers/drawbar/pkg/workflow"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// cliFlags holds the parsed command-line flags.
type cliFlags struct {
	ConfigPath      string
	CredentialFile  string
	SecretName      string
	SecretNamespace string
	Kubeconfig      string
	JobNamespace    string
}

// parseFlags registers and parses flags on the given FlagSet.
func parseFlags(fs *flag.FlagSet, args []string) (*cliFlags, error) {
	f := &cliFlags{}
	fs.StringVar(&f.ConfigPath, "config", "config.yaml", "path to config file")
	fs.StringVar(&f.CredentialFile, "credential-file", "", "path to credential file (local dev; omit for k8s Secret)")
	fs.StringVar(&f.SecretName, "secret-name", "drawbar", "k8s Secret name for credentials")
	fs.StringVar(&f.SecretNamespace, "secret-namespace", "", "k8s namespace for credential Secret (default: current namespace)")
	fs.StringVar(&f.Kubeconfig, "kubeconfig", "", "path to kubeconfig (for out-of-cluster dev)")
	fs.StringVar(&f.JobNamespace, "job-namespace", "", "k8s namespace for job pods (default: same as credential namespace)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return f, nil
}

// resolveNamespace returns explicit if non-empty, otherwise fallback.
func resolveNamespace(explicit, fallback string) string {
	if explicit != "" {
		return explicit
	}
	return fallback
}

// createStore builds the appropriate credential store based on flags.
func createStore(credFile string, k8sClient kubernetes.Interface, namespace, secretName string) server.CredentialStore {
	if credFile != "" {
		slog.Info("using file credential store", "path", credFile)
		return &server.FileStore{Path: credFile}
	}
	slog.Info("using k8s secret credential store", "namespace", namespace, "name", secretName)
	return &server.SecretStore{
		Client:    k8sClient,
		Namespace: namespace,
		Name:      secretName,
	}
}

func main() {
	flags, err := parseFlags(flag.CommandLine, os.Args[1:])
	if err != nil {
		slog.Error("failed to parse flags", "error", err)
		os.Exit(1)
	}

	// Load config.
	cfg, err := config.Load(flags.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Set up structured logging.
	logger := setupLogging(cfg.Log.Level)

	slog.Info("starting drawbar", "version", version.Full())

	if err := cfg.Validate(); err != nil {
		slog.Error("config validation failed", "error", err)
		os.Exit(1)
	}

	// Parse labels.
	parsedLabels, err := parseLabels(cfg.Runner.Labels)
	if err != nil {
		slog.Error("invalid label", "error", err)
		os.Exit(1)
	}
	slog.Info("labels configured", "labels", parsedLabels.Strings())

	// Create k8s client.
	k8sClient, restConfig, k8sNamespace, err := k8s.NewClient(flags.Kubeconfig, flags.SecretNamespace)
	if err != nil {
		slog.Error("failed to create k8s client", "error", err)
		os.Exit(1)
	}

	jobNS := resolveNamespace(flags.JobNamespace, k8sNamespace)
	store := createStore(flags.CredentialFile, k8sClient, k8sNamespace, flags.SecretName)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := run(ctx, cfg, runDeps{
		k8sClient:  k8sClient,
		restConfig: restConfig,
		store:      store,
		labels:     parsedLabels,
		namespace:  jobNS,
		logger:     logger,
	}); err != nil {
		slog.Error("fatal error", "error", err)
		os.Exit(1)
	}
}

// runDeps holds pre-built dependencies that main() creates but run() consumes.
type runDeps struct {
	k8sClient  kubernetes.Interface
	restConfig *rest.Config
	store      server.CredentialStore
	labels     labels.Labels
	namespace  string
	logger     *slog.Logger
	watchCfg   k8s.WatchConfig // optional; zero value uses defaults
}

// run is the core controller loop, testable without flag parsing or signal setup.
func run(ctx context.Context, cfg *config.Config, deps runDeps) error {
	// Register or reconnect.
	serverClient, err := server.EnsureRegistered(ctx, server.RegisterConfig{
		Endpoint:          cfg.Server.URL,
		Insecure:          cfg.Server.Insecure,
		RegistrationToken: cfg.Server.RegistrationToken,
		Name:              cfg.Runner.Name,
		Labels:            deps.labels.Names(),
		Version:           version.Full(),
		FetchInterval:     cfg.Runner.FetchInterval,
		HTTPTimeout:       cfg.Runner.FetchTimeout,
		Store:             deps.store,
	})
	if err != nil {
		return fmt.Errorf("registration failed: %w", err)
	}

	// Start cache server if enabled.
	var cacheProxy *cacheproxy.Handler
	if cfg.Cache.Enabled {
		cacheProxy, err = startCacheServer(cfg.Cache, deps.store, ctx)
		if err != nil {
			return fmt.Errorf("starting cache server: %w", err)
		}
		slog.Info("cache server started", "proxy_port", cfg.Cache.Port)
	}

	// Set up action repo cache.
	actionCache := actions.NewActionCache(cfg.Cache.Dir)

	// Set up snapshot manager if enabled.
	var snapMgr *snapshot.Manager
	if cfg.Snapshot.Enabled && deps.restConfig != nil {
		snapClient, err := snapshotclient.NewForConfig(deps.restConfig)
		if err != nil {
			return fmt.Errorf("creating snapshot client: %w", err)
		}
		snapMgr = &snapshot.Manager{
			K8sClient:      deps.k8sClient,
			SnapshotClient: snapClient,
			Namespace:      deps.namespace,
			SnapshotClass:  cfg.Snapshot.Class,
			StorageClass:   cfg.Snapshot.StorageClass,
			PVCSize:        cfg.Snapshot.Size,
			RetentionDays:  cfg.Snapshot.RetentionDays,
		}
		slog.Info("ZFS snapshot cache enabled",
			"class", cfg.Snapshot.Class,
			"storage_class", cfg.Snapshot.StorageClass,
			"size", cfg.Snapshot.Size,
			"retention_days", cfg.Snapshot.RetentionDays,
		)

		// Start GC goroutine.
		go func() {
			ticker := time.NewTicker(1 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					deleted, err := snapMgr.GarbageCollect(ctx)
					if err != nil {
						slog.Warn("snapshot GC error", "error", err)
					} else if deleted > 0 {
						slog.Info("snapshot GC completed", "deleted", deleted)
					}
				}
			}
		}()
	}

	// Create task handler.
	handler := makeTaskHandler(TaskHandlerConfig{
		K8sClient:        deps.k8sClient,
		RestConfig:       deps.restConfig,
		ServerClient:    serverClient,
		Labels:           deps.labels,
		Namespace:        deps.namespace,
		Timeout:          cfg.Runner.Timeout,
		GitCloneURL:      cfg.Runner.GitCloneURL,
		ActionsURL:       cfg.Runner.ActionsURL,
		ControllerImage:  cfg.Runner.ControllerImage,
		CacheProxy:       cacheProxy,
		CachePort:        cfg.Cache.Port,
		CacheServiceName: cfg.Cache.ServiceName,
		CachePVCName:     cfg.Cache.PVCName,
		JobSecrets:       cfg.Runner.JobSecrets,
		ActionCache:      actionCache,
		WatchConfig:      deps.watchCfg,
		SnapshotManager:  snapMgr,
	})

	// Create poller.
	poller := server.NewPoller(
		serverClient,
		handler,
		int64(cfg.Runner.Capacity),
		cfg.Runner.FetchTimeout,
		cfg.Runner.Ephemeral,
		deps.logger,
	)

	// Clean up orphaned jobs.
	cleanupOrphanedJobs(ctx, deps.k8sClient, deps.namespace)

	// Start health server.
	var registered atomic.Bool
	registered.Store(true)
	go startHealthServer(&registered)

	slog.Info("runner is online, polling for tasks", "job_namespace", deps.namespace)
	poller.Run(ctx)
	slog.Info("poller stopped, draining in-flight tasks")
	poller.Drain(30 * time.Second)
	slog.Info("runner shut down")
	return nil
}

func setupLogging(level string) *slog.Logger {
	var logLevel slog.Level
	switch level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)
	return logger
}

func parseLabels(rawLabels []string) (labels.Labels, error) {
	var parsed labels.Labels
	for _, l := range rawLabels {
		label, err := labels.Parse(l)
		if err != nil {
			return nil, fmt.Errorf("invalid label %q: %w", l, err)
		}
		parsed = append(parsed, label)
	}
	return parsed, nil
}

func cleanupOrphanedJobs(ctx context.Context, client kubernetes.Interface, namespace string) {
	jobs, err := client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=drawbar",
	})
	if err != nil {
		slog.Warn("failed to list orphaned jobs", "error", err)
		return
	}

	propagation := metav1.DeletePropagationBackground
	for _, job := range jobs.Items {
		if job.Status.Active > 0 {
			slog.Info("cleaning up orphaned job", "job", job.Name)
			_ = client.BatchV1().Jobs(namespace).Delete(ctx, job.Name, metav1.DeleteOptions{
				PropagationPolicy: &propagation,
			})
		}
	}
}

func startHealthServer(registered *atomic.Bool) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler)
	mux.HandleFunc("/readyz", readyzHandler(registered))
	slog.Info("health server listening", "port", 8081)
	if err := http.ListenAndServe(":8081", mux); err != nil {
		slog.Error("health server error", "error", err)
	}
}

func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func readyzHandler(registered *atomic.Bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if registered.Load() {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("not ready"))
		}
	}
}

func startCacheServer(cfg config.CacheConfig, store server.CredentialStore, ctx context.Context) (*cacheproxy.Handler, error) {
	// Load or generate cache secret.
	reg, err := store.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading credentials for cache secret: %w", err)
	}

	secret := ""
	if reg != nil {
		secret = reg.CacheSecret
	}
	if secret == "" {
		secretBytes := make([]byte, 64)
		if _, err := rand.Read(secretBytes); err != nil {
			return nil, fmt.Errorf("generating cache secret: %w", err)
		}
		secret = hex.EncodeToString(secretBytes)

		// Persist the secret.
		if reg != nil {
			reg.CacheSecret = secret
			if err := store.Save(ctx, reg); err != nil {
				slog.Warn("failed to persist cache secret", "error", err)
			}
		}
	}

	// Ensure cache dir exists.
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating cache dir %s: %w", cfg.Dir, err)
	}

	// Start internal cache server (BoltDB + filesystem storage).
	logrusLogger := newLogrusLogger()
	cacheHandler, err := artifactcache.StartHandler(cfg.Dir, "", 0, secret, logrusLogger)
	if err != nil {
		return nil, fmt.Errorf("starting cache handler: %w", err)
	}
	slog.Info("cache storage server started", "url", cacheHandler.ExternalURL())

	// Start cache proxy (per-job auth layer).
	proxy, err := cacheproxy.StartHandler(
		cacheHandler.ExternalURL(), // target: internal cache server
		"",                         // outbound IP: auto-detect
		cfg.Port,                   // listen port
		"",                         // no host override
		secret,                     // HMAC secret
		logrusLogger,               // logger
		cacheHandler,               // closer: shut down cache when proxy shuts down
	)
	if err != nil {
		cacheHandler.Close()
		return nil, fmt.Errorf("starting cache proxy: %w", err)
	}

	return proxy, nil
}

// newLogrusLogger creates a logrus logger for cache packages (which require logrus).
func newLogrusLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(os.Stdout)
	l.SetFormatter(&logrus.JSONFormatter{})
	return l
}

// TaskHandlerConfig holds all dependencies for the task handler.
type TaskHandlerConfig struct {
	K8sClient        kubernetes.Interface
	RestConfig       *rest.Config
	ServerClient    *server.Client
	Labels           labels.Labels
	Namespace        string
	Timeout          time.Duration
	GitCloneURL      string
	ActionsURL       string
	ControllerImage  string
	CacheProxy       *cacheproxy.Handler
	CachePort        uint16
	CacheServiceName string
	CachePVCName     string
	JobSecrets       []config.JobSecret
	ActionCache      *actions.ActionCache
	WatchConfig      k8s.WatchConfig    // optional; zero value uses defaults
	SnapshotManager  *snapshot.Manager   // optional; nil = no ZFS snapshot cache
}

// makeTaskHandler returns a TaskHandler that executes workflow jobs as k8s Jobs.
func makeTaskHandler(cfg TaskHandlerConfig) server.TaskHandler {
	return func(ctx context.Context, task *runnerv1.Task) {
		slog.Info("executing task", "id", task.GetId())

		// Parse workflow.
		parsed, err := workflow.ParseTask(task)
		if err != nil {
			slog.Error("failed to parse workflow", "task_id", task.GetId(), "error", err)
			reportFailure(ctx, cfg.ServerClient, task, fmt.Sprintf("Failed to parse workflow: %v", err))
			return
		}

		slog.Info("parsed workflow",
			"task_id", task.GetId(),
			"job_id", parsed.JobID,
			"steps", len(parsed.Steps),
			"runs_on", parsed.RunsOn,
		)

		image := resolveJobImage(cfg.Labels, parsed.RunsOn, parsed.Container)
		slog.Info("resolved image", "image", image)

		// Task context fields (used for action URL resolution, cache, etc.)
		taskCtx := task.GetContext().GetFields()
		var actionsToClone []*actions.ActionMeta

		// Create expression evaluator for interpolating scripts and env values.
		// Note: if: conditions are NOT evaluated here — they are passed through
		// to the entrypoint for runtime evaluation (supports failure()/always()/steps.*).
		evalEnv := expressions.BuildEnvironment(task, parsed.Env)
		eval := expressions.NewEvaluator(evalEnv)

		// Build step specs — all steps are included, with raw if: expressions.
		steps := make([]types.StepSpec, 0, len(parsed.Steps))
		for _, step := range parsed.Steps {
			stepID := step.ID
			if stepID == "" {
				stepID = fmt.Sprintf("step-%d", len(steps))
			}

			// Capture the raw if: expression for runtime evaluation.
			ifExpr := ""
			if step.If.Value != "" {
				ifExpr = step.If.Value
			}

			timeout := parseTimeoutMinutes(step.TimeoutMinutes)
			continueOnError := strings.EqualFold(step.RawContinueOnError, "true")

			if step.Run != "" {
				// run: step — interpolate expressions in script and env.
				script := eval.Interpolate(step.Run)
				env := eval.InterpolateMap(step.GetEnv())

				name := step.Name
				if name == "" {
					name = fmt.Sprintf("Run %s", truncate(step.Run, 40))
				}
				steps = append(steps, types.StepSpec{
					ID:              stepID,
					Name:            name,
					Shell:           step.RawShell,
					Script:          script,
					Env:             env,
					If:              ifExpr,
					ContinueOnError: continueOnError,
					TimeoutMinutes:  timeout,
				})
			} else if workflow.IsCheckout(step) {
				// uses: actions/checkout — built-in git clone via structured args
				spec, err := workflow.ResolveCheckout(task, step, cfg.GitCloneURL)
				if err != nil {
					slog.Error("failed to resolve checkout", "error", err)
					reportFailure(ctx, cfg.ServerClient, task, fmt.Sprintf("Checkout error: %v", err))
					return
				}
				checkoutSpecs := spec.ToStepSpecs()
				for j, cs := range checkoutSpecs {
					csID := stepID
					if j > 0 {
						csID = fmt.Sprintf("%s-checkout-%d", stepID, j)
					}
					csIf := ""
					if j == 0 {
						csIf = ifExpr // only first checkout sub-step gets the condition
					}
					steps = append(steps, types.StepSpec{
						ID:              csID,
						Name:            cs.Name,
						Args:            cs.Args,
						Env:             cs.Env,
						If:              csIf,
						ContinueOnError: continueOnError,
						TimeoutMinutes:  timeout,
					})
				}
			} else if step.Uses != "" {
				// Generic action (node, docker, composite, go, sh).
				ref, err := actions.ParseActionRef(step.Uses)
				if err != nil {
					slog.Warn("unsupported action reference", "uses", step.Uses, "error", err)
					continue
				}

				// Resolve default actions URL.
				resolvedActionsURL := cfg.ActionsURL
				if resolvedActionsURL == "" {
					resolvedActionsURL = taskCtx["gitea_default_actions_url"].GetStringValue()
				}
				if resolvedActionsURL == "" {
					resolvedActionsURL = "https://code.forgejo.org"
				}

				token := taskCtx["token"].GetStringValue()
				meta, err := cfg.ActionCache.LoadAction(ref, resolvedActionsURL, token)
				if err != nil {
					slog.Error("failed to load action", "action", ref.String(), "error", err)
					reportFailure(ctx, cfg.ServerClient, task, fmt.Sprintf("Failed to load action %s: %v", ref.String(), err))
					return
				}

				actionSpecs, err := meta.ToStepSpecs(step.With, step.GetEnv())
				if err != nil {
					slog.Error("failed to build action steps", "action", ref.String(), "error", err)
					reportFailure(ctx, cfg.ServerClient, task, fmt.Sprintf("Failed to build action %s: %v", ref.String(), err))
					return
				}

				for j, as := range actionSpecs {
					asID := stepID
					if j > 0 {
						asID = fmt.Sprintf("%s-%d", stepID, j)
					}
					asIf := ""
					if j == 0 {
						asIf = ifExpr
					}
					steps = append(steps, types.StepSpec{
						ID:              asID,
						Name:            as.Name,
						Shell:           as.Shell,
						Script:          as.Script,
						Env:             as.Env,
						ActionDir:       meta.Dir,
						If:              asIf,
						ContinueOnError: continueOnError,
						TimeoutMinutes:  timeout,
					})
				}
				actionsToClone = append(actionsToClone, meta)
			}
		}

		if len(steps) == 0 {
			slog.Warn("no executable steps", "task_id", task.GetId())
			reportFailure(ctx, cfg.ServerClient, task, "No executable steps found")
			return
		}

		// Create reporter with secret masking.
		rep := reporter.New(cfg.ServerClient, task.GetId(), len(steps), 1*time.Second)
		rep.SetSecrets(collectSecrets(task))
		rep.RunDaemon(ctx)

		// Build k8s Job.
		runID := ""
		if task.GetContext() != nil {
			runID = task.GetContext().GetFields()["run_id"].GetStringValue()
		}

		var timeoutSecs int64
		if cfg.Timeout > 0 {
			timeoutSecs = int64(cfg.Timeout.Seconds())
		}

		services := convertServices(parsed.Services)
		if len(services) > 0 {
			slog.Info("configured services", "count", len(services))
		}

		// Build base env (shared across all steps via manifest).
		baseEnv := make(map[string]string)

		// Propagate job-level env: vars so they are available as real
		// environment variables in every step (not just for expression interpolation).
		for k, v := range parsed.Env {
			baseEnv[k] = v
		}

		// Inject cache URL if cache is enabled.
		if cfg.CacheProxy != nil {
			repository := taskCtx["repository"].GetStringValue()
			runIDStr := taskCtx["run_id"].GetStringValue()
			eventName := taskCtx["event_name"].GetStringValue()
			ref := taskCtx["ref"].GetStringValue()
			runtimeToken := taskCtx["gitea_runtime_token"].GetStringValue()

			writeIsolationKey := ""
			if eventName == "pull_request" || eventName == "pull_request_target" {
				writeIsolationKey = ref
			}

			timestamp := strconv.FormatInt(time.Now().Unix(), 10)
			runData := cfg.CacheProxy.CreateRunData(repository, runIDStr, timestamp, writeIsolationKey)
			cacheRunID, err := cfg.CacheProxy.AddRun(runData)
			if err != nil {
				slog.Warn("failed to register cache run", "error", err)
			} else {
				cacheURL := fmt.Sprintf("http://%s.%s.svc:%d/%s/",
					cfg.CacheServiceName, cfg.Namespace, cfg.CachePort, cacheRunID)
				baseEnv["ACTIONS_CACHE_URL"] = cacheURL
				if runtimeToken != "" {
					baseEnv["ACTIONS_RUNTIME_TOKEN"] = runtimeToken
				}
				slog.Info("cache URL injected", "url", cacheURL)
			}
		}

		// Artifact server URL (Forgejo handles artifacts server-side).
		buildArtifactEnv(baseEnv, taskCtx["server_url"].GetStringValue(), cfg.GitCloneURL)

		// Determine cache PVC name.
		jobCachePVCName := ""
		if len(actionsToClone) > 0 {
			jobCachePVCName = cfg.CachePVCName
		}

		secretMounts := convertJobSecrets(cfg.JobSecrets)

		// Build evaluation context for runtime if: conditions.
		evalCtx := buildEvalContext(task, parsed.Env)

		// ZFS snapshot cache: look up or create workspace PVC.
		workspacePVCName := ""
		var snapshotCacheKey string
		if cfg.SnapshotManager != nil {
			snapshotCacheKey = extractCacheKey(steps, eval)
			if snapshotCacheKey != "" {
				repository := taskCtx["repository"].GetStringValue()
				pvcName := fmt.Sprintf("ws-%d", task.GetId())
				snap, err := cfg.SnapshotManager.FindSnapshot(ctx, repository, snapshotCacheKey)
				if err != nil {
					slog.Warn("snapshot lookup failed", "error", err)
				}
				if snap != nil {
					_, err = cfg.SnapshotManager.CreatePVCFromSnapshot(ctx, snap, pvcName)
				} else {
					_, err = cfg.SnapshotManager.CreateEmptyPVC(ctx, pvcName)
				}
				if err != nil {
					slog.Warn("failed to create workspace PVC, falling back to emptyDir", "error", err)
				} else {
					workspacePVCName = pvcName
				}
			}
		}

		k8sJob, err := k8s.BuildJob(k8s.JobConfig{
			TaskID:           task.GetId(),
			RunID:            runID,
			JobName:          parsed.JobID,
			Namespace:        cfg.Namespace,
			Image:            image,
			ControllerImage:  cfg.ControllerImage,
			Steps:            steps,
			BaseEnv:          baseEnv,
			Services:         services,
			Timeout:          timeoutSecs,
			CachePVCName:     jobCachePVCName,
			JobSecrets:       secretMounts,
			EvalContext:      evalCtx,
			WorkspacePVCName: workspacePVCName,
		})
		if err != nil {
			slog.Error("failed to build k8s job", "error", err)
			rep.AddLog(fmt.Sprintf("Failed to build k8s job: %v", err))
			rep.Close(ctx, runnerv1.Result_RESULT_FAILURE)
			return
		}

		// Create the Job.
		created, err := cfg.K8sClient.BatchV1().Jobs(cfg.Namespace).Create(ctx, k8sJob, metav1.CreateOptions{})
		if err != nil {
			slog.Error("failed to create k8s job", "error", err)
			rep.AddLog(fmt.Sprintf("Failed to create k8s job: %v", err))
			rep.Close(ctx, runnerv1.Result_RESULT_FAILURE)
			return
		}

		slog.Info("created k8s job", "job", created.Name, "namespace", cfg.Namespace)

		// Watch and stream.
		watchCfg := cfg.WatchConfig
		if watchCfg.PollInterval == 0 {
			watchCfg = k8s.DefaultWatchConfig()
		}
		debugEnabled := task.GetSecrets()["ACTIONS_STEP_DEBUG"] == "true"
		watchCfg.CommandProc = reporter.NewCommandProcessor(rep, debugEnabled)
		result, err := k8s.WatchJob(ctx, cfg.K8sClient, cfg.RestConfig, cfg.Namespace, created.Name, rep, watchCfg)
		if err != nil {
			slog.Error("job watch error", "error", err)
			rep.AddLog(fmt.Sprintf("Job execution error: %v", err))
			result = runnerv1.Result_RESULT_FAILURE
		}

		// Report final result.
		if err := rep.Close(ctx, result); err != nil {
			slog.Error("failed to report final result", "error", err)
		}

		// ZFS snapshot cache: snapshot on success, always delete PVC.
		if workspacePVCName != "" && cfg.SnapshotManager != nil {
			if result == runnerv1.Result_RESULT_SUCCESS && snapshotCacheKey != "" {
				repository := taskCtx["repository"].GetStringValue()
				snapName := fmt.Sprintf("snap-%d", task.GetId())
				if _, err := cfg.SnapshotManager.SnapshotPVC(ctx, workspacePVCName, snapName, repository, snapshotCacheKey); err != nil {
					slog.Warn("failed to snapshot workspace", "error", err)
				}
			}
			if err := cfg.SnapshotManager.DeletePVC(ctx, workspacePVCName); err != nil {
				slog.Warn("failed to delete workspace PVC", "error", err)
			}
		}

		slog.Info("task completed", "task_id", task.GetId(), "result", result)
	}
}

// reportFailure sends a simple failure report for a task that couldn't be executed.
func reportFailure(ctx context.Context, client *server.Client, task *runnerv1.Task, message string) {
	rep := reporter.New(client, task.GetId(), 0, 1*time.Second)
	rep.AddLog(message)
	if err := rep.Close(ctx, runnerv1.Result_RESULT_FAILURE); err != nil {
		slog.Error("failed to report failure", "task_id", task.GetId(), "error", err)
	}
}

// buildEvalContext creates a serializable evaluation context for the entrypoint.
func buildEvalContext(task *runnerv1.Task, jobEnv map[string]string) *types.EvalContext {
	needs := make(map[string]exprparser.Needs)
	for id, need := range task.GetNeeds() {
		needs[id] = exprparser.Needs{
			Outputs: need.GetOutputs(),
			Result:  strings.ToLower(strings.TrimPrefix(need.GetResult().String(), "RESULT_")),
		}
	}
	return &types.EvalContext{
		GitHub:  expressions.BuildGithubContext(task),
		Env:     jobEnv,
		Secrets: task.GetSecrets(),
		Vars:    task.GetVars(),
		Needs:   needs,
	}
}

// resolveJobImage picks the container image from labels or job-level override.
func resolveJobImage(l labels.Labels, runsOn []string, container *model.ContainerSpec) string {
	image := l.PickPlatform(runsOn)
	if container != nil && container.Image != "" {
		image = container.Image
	}
	return image
}

// collectSecrets extracts secret values from a task for log masking.
func collectSecrets(task *runnerv1.Task) []string {
	var secrets []string
	for _, v := range task.GetSecrets() {
		secrets = append(secrets, v)
	}
	taskCtx := task.GetContext().GetFields()
	if token := taskCtx["token"].GetStringValue(); token != "" {
		secrets = append(secrets, token)
	}
	if rt := taskCtx["gitea_runtime_token"].GetStringValue(); rt != "" {
		secrets = append(secrets, rt)
	}
	return secrets
}

// convertServices converts parsed workflow services into k8s ServiceSpecs.
func convertServices(services map[string]*model.ContainerSpec) []k8s.ServiceSpec {
	var result []k8s.ServiceSpec
	for name, spec := range services {
		if spec == nil || spec.Image == "" {
			continue
		}
		svc := k8s.ServiceSpec{
			Name:  name,
			Image: spec.Image,
			Env:   spec.Env,
			Cmd:   spec.Cmd,
		}
		for _, portStr := range spec.Ports {
			port, err := k8s.ParseContainerPort(portStr)
			if err != nil {
				slog.Warn("invalid service port", "service", name, "port", portStr, "error", err)
				continue
			}
			svc.Ports = append(svc.Ports, port)
		}
		result = append(result, svc)
	}
	return result
}

// buildArtifactEnv sets ACTIONS_RUNTIME_URL and ACTIONS_RESULTS_URL in the env map.
func buildArtifactEnv(env map[string]string, serverURL, gitCloneURL string) {
	forgejoURL := strings.TrimSuffix(serverURL, "/")
	if gitCloneURL != "" {
		forgejoURL = strings.TrimSuffix(gitCloneURL, "/")
	}
	env["ACTIONS_RUNTIME_URL"] = forgejoURL + "/api/actions_pipeline/"
	env["ACTIONS_RESULTS_URL"] = forgejoURL + "/"
}

// convertJobSecrets converts config JobSecrets to k8s builder format.
func convertJobSecrets(secrets []config.JobSecret) []k8s.JobSecretMount {
	var mounts []k8s.JobSecretMount
	for _, s := range secrets {
		mounts = append(mounts, k8s.JobSecretMount{
			Name:      s.Name,
			MountPath: s.MountPath,
		})
	}
	return mounts
}

// extractCacheKey finds the cache key from actions/cache steps in the workflow.
// Returns empty string if no cache step is found.
func extractCacheKey(steps []types.StepSpec, eval *expressions.Evaluator) string {
	for _, step := range steps {
		// The step name for actions/cache typically contains "actions/cache".
		// But we can't rely on the name since it's been transformed.
		// Instead, check the env for INPUT_KEY which is set by buildActionEnv.
		if key, ok := step.Env["INPUT_KEY"]; ok && key != "" {
			return key
		}
	}
	return ""
}

// parseTimeoutMinutes converts a string timeout-minutes value to float64.
// Returns 0 for empty, invalid, or non-positive values.
func parseTimeoutMinutes(s string) float64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}
