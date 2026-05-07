package server

import (
	"context"
	"time"

	runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
	"connectrpc.com/connect"
	gouuid "github.com/google/uuid"
)

// ReporterClient is the subset of the runner protocol needed by the reporter.
type ReporterClient interface {
	UpdateLog(context.Context, *connect.Request[runnerv1.UpdateLogRequest]) (*connect.Response[runnerv1.UpdateLogResponse], error)
	UpdateTask(context.Context, *connect.Request[runnerv1.UpdateTaskRequest]) (*connect.Response[runnerv1.UpdateTaskResponse], error)
}

// PollerClient is the subset of the runner protocol needed by the poller.
type PollerClient interface {
	FetchTask(context.Context, *connect.Request[runnerv1.FetchTaskRequest]) (*connect.Response[runnerv1.FetchTaskResponse], error)
	Endpoint() string
	FetchInterval() time.Duration
	SetRequestKey(gouuid.UUID) func()
}
