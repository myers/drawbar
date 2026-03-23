package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/myers/drawbar/pkg/expressions"
)

const shimDir = "/shim"

// runCommand is the function used to execute commands. Tests can override it.
var runCommand = func(cmd *exec.Cmd) error { return cmd.Run() }

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: entrypoint <manifest.json>\n")
		os.Exit(1)
	}

	if !runEntrypoint(os.Args[1], shimDir) {
		os.Exit(1)
	}
}

// runEntrypoint executes all steps in the manifest. Returns true on success.
func runEntrypoint(manifestPath, workDir string) bool {
	manifest, err := loadManifest(manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return false
	}

	stateFile, err := os.OpenFile(filepath.Join(workDir, "state.jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening state file: %v\n", err)
		return false
	}
	defer stateFile.Close()

	// Set up expression evaluator for runtime if: conditions.
	var eval *expressions.Evaluator
	if manifest.Context != nil {
		eval = expressions.NewEvaluator(manifest.Context.BuildEvalEnv())
	}

	// Check if any step has a runtime condition — if so, we must continue
	// past failures to let later steps evaluate their conditions.
	hasRuntimeConditions := false
	for _, step := range manifest.Steps {
		if step.If != "" {
			hasRuntimeConditions = true
			break
		}
	}

	accumulatedEnv := make(map[string]string)
	accumulatedPaths := []string{}
	stepResults := make(map[string]StepResult)
	overallSuccess := true
	hadFailure := false

	for i, step := range manifest.Steps {
		// Evaluate runtime if: condition.
		if step.If != "" && eval != nil {
			shouldRun, err := eval.EvalCondition(step.If)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to evaluate if condition for step %d (%s): %v\n", i, step.Name, err)
				shouldRun = false
			}
			if !shouldRun {
				writeState(stateFile, StateEvent{
					Event: "skip",
					Step:  i,
					Name:  step.Name,
					Time:  time.Now().UTC().Format(time.RFC3339),
				})
				fmt.Fprintf(os.Stderr, "Step %d (%s) skipped (condition: %s)\n", i, step.Name, step.If)
				continue
			}
		}

		// If a previous step failed and this step has no condition,
		// the default implicit condition is success() — skip it.
		if hadFailure && step.If == "" {
			writeState(stateFile, StateEvent{
				Event: "skip",
				Step:  i,
				Name:  step.Name,
				Time:  time.Now().UTC().Format(time.RFC3339),
			})
			continue
		}

		// Write step start event.
		writeState(stateFile, StateEvent{
			Event: "start",
			Step:  i,
			Name:  step.Name,
			Time:  time.Now().UTC().Format(time.RFC3339),
		})

		// Set up per-step files.
		outputFile := filepath.Join(workDir, fmt.Sprintf("output-%d", i))
		envFile := filepath.Join(workDir, fmt.Sprintf("env-%d", i))
		pathFile := filepath.Join(workDir, fmt.Sprintf("path-%d", i))
		stateStepFile := filepath.Join(workDir, fmt.Sprintf("state-%d", i))
		summaryFile := filepath.Join(workDir, fmt.Sprintf("summary-%d", i))

		// Create empty files so steps can append.
		for _, f := range []string{outputFile, envFile, pathFile, stateStepFile, summaryFile} {
			if err := os.WriteFile(f, nil, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "error creating step file %s: %v\n", f, err)
				return false
			}
		}

		// Build environment: base + accumulated + step-specific.
		env := buildStepEnv(manifest.BaseEnv, accumulatedEnv, step.Env, accumulatedPaths)
		env["GITHUB_OUTPUT"] = outputFile
		env["FORGEJO_OUTPUT"] = outputFile
		env["GITHUB_ENV"] = envFile
		env["FORGEJO_ENV"] = envFile
		env["GITHUB_PATH"] = pathFile
		env["FORGEJO_PATH"] = pathFile
		env["GITHUB_STATE"] = stateStepFile
		env["FORGEJO_STATE"] = stateStepFile
		env["GITHUB_STEP_SUMMARY"] = summaryFile
		env["FORGEJO_STEP_SUMMARY"] = summaryFile
		env["GITHUB_WORKSPACE"] = "/workspace"
		env["FORGEJO_WORKSPACE"] = "/workspace"

		// Execute the step.
		// Apply per-step timeout if configured.
		stepCtx := context.Background()
		var cancelTimeout context.CancelFunc
		if step.TimeoutMinutes > 0 {
			stepCtx, cancelTimeout = context.WithTimeout(stepCtx,
				time.Duration(step.TimeoutMinutes*float64(time.Minute)))
		}

		exitCode := executeStep(stepCtx, step, env)

		if cancelTimeout != nil {
			cancelTimeout()
		}

		// Read outputs from the step.
		outputs, _ := parseEnvFile(outputFile)
		newEnv, _ := parseEnvFile(envFile)
		newPaths, _ := parsePaths(pathFile)

		// Accumulate state for next steps.
		for k, v := range newEnv {
			accumulatedEnv[k] = v
		}
		accumulatedPaths = append(accumulatedPaths, newPaths...)

		// Record result.
		if outputs == nil {
			outputs = make(map[string]string)
		}
		stepResults[step.ID] = StepResult{
			ExitCode: exitCode,
			Outputs:  outputs,
		}

		// Update expression evaluator with step result.
		if eval != nil {
			outcome := "success"
			if exitCode != 0 {
				outcome = "failure"
				// continue-on-error masks the failure: job status stays "success".
				if !step.ContinueOnError {
					eval.SetJobStatus("failure")
				}
			}
			eval.SetStepResult(step.ID, outcome, outputs)
		}

		// Write step end event.
		writeState(stateFile, StateEvent{
			Event:    "end",
			Step:     i,
			Name:     step.Name,
			ExitCode: exitCode,
			Time:     time.Now().UTC().Format(time.RFC3339),
		})

		if exitCode != 0 {
			if step.ContinueOnError {
				fmt.Fprintf(os.Stderr, "Step %d (%s) failed with exit code %d (continue-on-error)\n", i, step.Name, exitCode)
			} else {
				fmt.Fprintf(os.Stderr, "Step %d (%s) failed with exit code %d\n", i, step.Name, exitCode)
				hadFailure = true
				overallSuccess = false
				if !hasRuntimeConditions {
					// No runtime conditions — break immediately (old behavior).
					break
				}
				// Continue to let subsequent steps with if: conditions evaluate.
			}
		}
	}

	// Write final results.
	resultsData, err := json.MarshalIndent(stepResults, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error marshaling results: %v\n", err)
	} else {
		if err := os.WriteFile(filepath.Join(workDir, "results.json"), resultsData, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to write results.json: %v\n", err)
		}
	}

	return overallSuccess
}

func buildStepEnv(base, accumulated, stepEnv map[string]string, extraPaths []string) map[string]string {
	env := make(map[string]string)

	// Start with current process env (includes system PATH etc.).
	for _, e := range os.Environ() {
		if idx := strings.IndexByte(e, '='); idx > 0 {
			env[e[:idx]] = e[idx+1:]
		}
	}

	// Layer: base env from manifest.
	for k, v := range base {
		env[k] = v
	}

	// Layer: accumulated env from previous steps ($GITHUB_ENV).
	for k, v := range accumulated {
		env[k] = v
	}

	// Layer: step-specific env.
	for k, v := range stepEnv {
		env[k] = v
	}

	// Prepend accumulated paths.
	if len(extraPaths) > 0 {
		currentPath := env["PATH"]
		env["PATH"] = strings.Join(extraPaths, ":") + ":" + currentPath
	}

	return env
}

func executeStep(ctx context.Context, step StepDef, env map[string]string) int {
	var cmd *exec.Cmd

	if len(step.Args) > 0 {
		// Direct exec mode: no shell involved, immune to injection.
		cmd = exec.CommandContext(ctx, step.Args[0], step.Args[1:]...)
	} else {
		// Shell mode for user-defined run: steps.
		shell := step.Shell
		if shell == "" {
			shell = "sh"
		}

		switch shell {
		case "bash":
			cmd = exec.CommandContext(ctx, "/bin/bash", "-e", "-c", step.Command)
		case "python":
			cmd = exec.CommandContext(ctx, "python3", "-c", step.Command)
		default:
			cmd = exec.CommandContext(ctx, "/bin/sh", "-e", "-c", step.Command)
		}
	}

	cmd.Dir = step.WorkDir
	if cmd.Dir == "" {
		cmd.Dir = "/workspace"
	}

	// Set environment.
	cmd.Env = make([]string, 0, len(env))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	// Connect stdout/stderr to our stdout/stderr (streamed to controller).
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := runCommand(cmd); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			fmt.Fprintf(os.Stderr, "Step timed out after %.1f minutes\n", step.TimeoutMinutes)
			return 1
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "exec error: %v\n", err)
		return 1
	}
	return 0
}

func writeState(f *os.File, event StateEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to marshal state event: %v\n", err)
		return
	}
	f.Write(data)
	f.WriteString("\n")
	f.Sync()
}
