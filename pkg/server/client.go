// Adapted from gitea.com/gitea/act_runner internal/pkg/client/
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

	"code.gitea.io/actions-proto-go/ping/v1/pingv1connect"
	"code.gitea.io/actions-proto-go/runner/v1/runnerv1connect"
	"connectrpc.com/connect"
	gouuid "github.com/google/uuid"
	"golang.org/x/net/http2"
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
	transport, _ := buildTransport(endpoint, insecure)
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}

// buildTransport constructs the http.Transport and, separately, the
// http2.Transport configured on top of it. The *http2.Transport is returned
// so callers (including tests) can inspect or further configure PING knobs
// without having to dig through unexported fields.
func buildTransport(endpoint string, insecure bool) (*http.Transport, *http2.Transport) {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	if strings.HasPrefix(endpoint, "https://") && insecure {
		slog.Warn("TLS certificate verification disabled — connections are vulnerable to MITM attacks",
			"endpoint", endpoint)
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	// Enable HTTP/2 PINGs so a half-dead conn (NAT/LB drop, server crash without
	// FIN) is detected and torn within ~25s instead of waiting for OS TCP
	// keepalive (default 2h on Linux). See bugs/010.
	h2t, err := http2.ConfigureTransports(transport)
	if err != nil {
		slog.Error("http2 configure failed", "error", err)
		return transport, nil
	}
	h2t.ReadIdleTimeout = 15 * time.Second
	h2t.PingTimeout = 10 * time.Second
	h2t.WriteByteTimeout = 30 * time.Second

	return transport, h2t
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
