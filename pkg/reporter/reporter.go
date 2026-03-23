package reporter

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Client is the subset of the Forgejo runner protocol the reporter needs.
type Client interface {
	UpdateLog(context.Context, *connect.Request[runnerv1.UpdateLogRequest]) (*connect.Response[runnerv1.UpdateLogResponse], error)
	UpdateTask(context.Context, *connect.Request[runnerv1.UpdateTaskRequest]) (*connect.Response[runnerv1.UpdateTaskResponse], error)
}

// Reporter batches log rows and step state updates, sending them
// periodically to Forgejo via UpdateLog and UpdateTask.
type Reporter struct {
	client   Client
	taskID   int64
	state    *runnerv1.TaskState
	logRows  []*runnerv1.LogRow
	logOffset int
	mu       sync.Mutex
	interval time.Duration
	cancel   context.CancelFunc

	// currentStep tracks which step is currently active for log routing.
	currentStep int
	masker      *logMasker
}

// logMasker replaces secret values with *** in log output.
// It stores the raw pairs so new values can be added dynamically (::add-mask::).
type logMasker struct {
	pairs    []string
	replacer *strings.Replacer
}

func newLogMasker(secrets []string) *logMasker {
	var pairs []string
	for _, s := range secrets {
		// Skip secrets of 3 characters or fewer to avoid excessive false-positive
		// masking — very short values like "yes", "no", "1" appear frequently in
		// normal log output and masking them would make logs unreadable.
		if len(s) > 3 {
			pairs = append(pairs, s, "***")
		}
	}
	if len(pairs) == 0 {
		return &logMasker{}
	}
	return &logMasker{pairs: pairs, replacer: strings.NewReplacer(pairs...)}
}

// add appends a new secret value to the masker and rebuilds the replacer.
func (m *logMasker) add(value string) {
	m.pairs = append(m.pairs, value, "***")
	m.replacer = strings.NewReplacer(m.pairs...)
}

func (m *logMasker) mask(content string) string {
	if m == nil || m.replacer == nil {
		return content
	}
	return m.replacer.Replace(content)
}

// New creates a reporter for a task with the given number of steps.
func New(client Client, taskID int64, numSteps int, interval time.Duration) *Reporter {
	steps := make([]*runnerv1.StepState, numSteps)
	for i := range numSteps {
		steps[i] = &runnerv1.StepState{Id: int64(i)}
	}

	return &Reporter{
		client:      client,
		taskID:      taskID,
		interval:    interval,
		currentStep: -1,
		state: &runnerv1.TaskState{
			Id:    taskID,
			Steps: steps,
		},
	}
}

// SetSecrets configures log masking for the given secret values.
func (r *Reporter) SetSecrets(secrets []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.masker = newLogMasker(secrets)
}

// AddMask dynamically adds a secret value to the masker (for ::add-mask::).
func (r *Reporter) AddMask(value string) {
	if len(value) <= 3 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.masker == nil {
		r.masker = &logMasker{}
	}
	r.masker.add(value)
}

// AddLog appends a log row and associates it with the current step.
func (r *Reporter) AddLog(content string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Mask secret values.
	content = r.masker.mask(content)

	row := &runnerv1.LogRow{
		Time:    timestamppb.Now(),
		Content: content,
	}
	r.logRows = append(r.logRows, row)

	// Track log range for the current step.
	if r.currentStep >= 0 && r.currentStep < len(r.state.Steps) {
		step := r.state.Steps[r.currentStep]
		if step.LogLength == 0 {
			step.LogIndex = int64(r.logOffset + len(r.logRows) - 1)
		}
		step.LogLength++
	}
}

// StartStep marks a step as running.
func (r *Reporter) StartStep(stepIdx int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if stepIdx < 0 || stepIdx >= len(r.state.Steps) {
		return
	}

	r.currentStep = stepIdx
	step := r.state.Steps[stepIdx]
	step.StartedAt = timestamppb.Now()

	if r.state.StartedAt == nil {
		r.state.StartedAt = timestamppb.Now()
	}
}

// FinishStep marks a step as completed with a result.
func (r *Reporter) FinishStep(stepIdx int, result runnerv1.Result) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if stepIdx < 0 || stepIdx >= len(r.state.Steps) {
		return
	}

	step := r.state.Steps[stepIdx]
	step.Result = result
	step.StoppedAt = timestamppb.Now()
}

// Flush sends pending logs and state to Forgejo.
func (r *Reporter) Flush(ctx context.Context) error {
	if err := r.flushLogs(ctx, false); err != nil {
		return fmt.Errorf("flushing logs: %w", err)
	}
	if err := r.flushState(ctx); err != nil {
		return fmt.Errorf("flushing state: %w", err)
	}
	return nil
}

func (r *Reporter) flushLogs(ctx context.Context, noMore bool) error {
	r.mu.Lock()
	rows := make([]*runnerv1.LogRow, len(r.logRows))
	copy(rows, r.logRows)
	r.mu.Unlock()

	if len(rows) == 0 && !noMore {
		return nil
	}

	resp, err := r.client.UpdateLog(ctx, connect.NewRequest(&runnerv1.UpdateLogRequest{
		TaskId: r.taskID,
		Index:  int64(r.logOffset),
		Rows:   rows,
		NoMore: noMore,
	}))
	if err != nil {
		return err
	}

	ack := int(resp.Msg.GetAckIndex())

	r.mu.Lock()
	if ack >= r.logOffset {
		trimmed := ack - r.logOffset
		if trimmed > len(r.logRows) {
			trimmed = len(r.logRows)
		}
		r.logRows = r.logRows[trimmed:]
		r.logOffset = ack
	}
	r.mu.Unlock()

	return nil
}

func (r *Reporter) flushState(ctx context.Context) error {
	r.mu.Lock()
	// Clone the state for sending.
	state := &runnerv1.TaskState{
		Id:        r.state.Id,
		Result:    r.state.Result,
		StartedAt: r.state.StartedAt,
		StoppedAt: r.state.StoppedAt,
		Steps:     r.state.Steps,
	}
	r.mu.Unlock()

	resp, err := r.client.UpdateTask(ctx, connect.NewRequest(&runnerv1.UpdateTaskRequest{
		State: state,
	}))
	if err != nil {
		return err
	}

	// Check if the server cancelled the task.
	if resp.Msg.GetState() != nil && resp.Msg.GetState().GetResult() == runnerv1.Result_RESULT_CANCELLED {
		if r.cancel != nil {
			slog.Info("task cancelled by server", "task_id", r.taskID)
			r.cancel()
		}
	}

	return nil
}

// RunDaemon starts periodic flushing in the background.
func (r *Reporter) RunDaemon(ctx context.Context) {
	ctx, r.cancel = context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := r.Flush(ctx); err != nil {
					slog.Warn("reporter flush error", "error", err, "task_id", r.taskID)
				}
			}
		}
	}()
}

// Close sends the final result with all remaining logs.
func (r *Reporter) Close(ctx context.Context, jobResult runnerv1.Result) error {
	// Stop the daemon to prevent concurrent flushes during the final upload.
	if r.cancel != nil {
		r.cancel()
	}

	r.mu.Lock()
	r.state.Result = jobResult
	r.state.StoppedAt = timestamppb.Now()

	// Mark any unfinished steps as cancelled.
	for _, step := range r.state.Steps {
		if step.Result == runnerv1.Result_RESULT_UNSPECIFIED {
			step.Result = runnerv1.Result_RESULT_CANCELLED
			if jobResult == runnerv1.Result_RESULT_SKIPPED {
				step.Result = runnerv1.Result_RESULT_SKIPPED
			}
		}
	}
	r.mu.Unlock()

	// Retry final log + state upload with exponential backoff.
	var lastErr error
	backoff := 100 * time.Millisecond
	for attempt := range 10 {
		if err := r.flushLogs(ctx, true); err != nil {
			slog.Warn("final log flush failed, retrying",
				"attempt", attempt+1, "error", err)
			lastErr = err
			time.Sleep(backoff)
			backoff *= 2
			continue
		}

		if err := r.flushState(ctx); err != nil {
			slog.Warn("final state flush failed, retrying",
				"attempt", attempt+1, "error", err)
			lastErr = err
			time.Sleep(backoff)
			backoff *= 2
			continue
		}

		return nil
	}

	return fmt.Errorf("failed to send final report after retries: %w", lastErr)
}
