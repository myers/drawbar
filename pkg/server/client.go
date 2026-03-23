// Adapted from code.forgejo.org/forgejo/runner internal/pkg/client/
// Original: Copyright 2022 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"code.forgejo.org/forgejo/actions-proto/ping/v1/pingv1connect"
	"code.forgejo.org/forgejo/actions-proto/runner/v1/runnerv1connect"
	"connectrpc.com/connect"
	gouuid "github.com/google/uuid"
)

const (
	UUIDHeader       = "x-runner-uuid"
	TokenHeader      = "x-runner-token"
	RequestKeyHeader = "x-runner-request-key"
)

// Client wraps the Connect RPC clients for the Forgejo runner protocol.
type Client struct {
	pingv1connect.PingServiceClient
	runnerv1connect.RunnerServiceClient

	endpoint      string
	insecure      bool
	fetchInterval time.Duration

	mu         sync.Mutex
	requestKey *gouuid.UUID
}

// NewClient creates a Connect RPC client for a Forgejo instance.
// uuid and token may be empty for initial registration.
func NewClient(endpoint string, insecure bool, uuid, token string, fetchInterval, httpTimeout time.Duration) *Client {
	baseURL := strings.TrimRight(endpoint, "/") + "/api/actions"

	if httpTimeout == 0 {
		httpTimeout = 60 * time.Second
	}

	client := &Client{
		endpoint:      endpoint,
		insecure:      insecure,
		fetchInterval: fetchInterval,
	}

	opts := []connect.ClientOption{
		connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				if uuid != "" {
					req.Header().Set(UUIDHeader, uuid)
				}
				if token != "" {
					req.Header().Set(TokenHeader, token)
				}
				client.mu.Lock()
				key := client.requestKey
				client.mu.Unlock()
				if key != nil {
					req.Header().Set(RequestKeyHeader, key.String())
				}
				return next(ctx, req)
			}
		})),
	}

	httpClient := newHTTPClient(endpoint, insecure, httpTimeout)

	client.PingServiceClient = pingv1connect.NewPingServiceClient(httpClient, baseURL, opts...)
	client.RunnerServiceClient = runnerv1connect.NewRunnerServiceClient(httpClient, baseURL, opts...)

	return client
}

func newHTTPClient(endpoint string, insecure bool, timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
	}

	if strings.HasPrefix(endpoint, "https://") && insecure {
		slog.Warn("TLS certificate verification disabled — connections are vulnerable to MITM attacks",
			"endpoint", endpoint)
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}

// Endpoint returns the Forgejo instance URL.
func (c *Client) Endpoint() string {
	return c.endpoint
}

// FetchInterval returns the configured poll interval.
func (c *Client) FetchInterval() time.Duration {
	return c.fetchInterval
}

// SetRequestKey sets the idempotency key for FetchTask. Returns a cleanup func.
func (c *Client) SetRequestKey(uuid gouuid.UUID) func() {
	c.mu.Lock()
	c.requestKey = &uuid
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		c.requestKey = nil
		c.mu.Unlock()
	}
}
