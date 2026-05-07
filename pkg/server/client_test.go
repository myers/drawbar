package server

import (
	"crypto/tls"
	"net/http"
	"testing"
	"time"

	gouuid "github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClient_Endpoint(t *testing.T) {
	c := NewClient("https://gitea.example.com", false, "", "", time.Second, time.Second)
	assert.Equal(t, "https://gitea.example.com", c.Endpoint())
}

func TestNewClient_FetchInterval(t *testing.T) {
	c := NewClient("http://localhost", false, "", "", 5*time.Second, time.Second)
	assert.Equal(t, 5*time.Second, c.FetchInterval())
}

func TestSetRequestKey_SetAndCleanup(t *testing.T) {
	c := NewClient("http://localhost", false, "", "", time.Second, time.Second)
	key := gouuid.New()

	cleanup := c.SetRequestKey(key)
	c.mu.Lock()
	assert.NotNil(t, c.requestKey)
	assert.Equal(t, key, *c.requestKey)
	c.mu.Unlock()

	cleanup()
	c.mu.Lock()
	assert.Nil(t, c.requestKey)
	c.mu.Unlock()
}

func TestNewHTTPClient_Default(t *testing.T) {
	hc := newHTTPClient("http://localhost", false, 10*time.Second)
	require.NotNil(t, hc)
	assert.Equal(t, 10*time.Second, hc.Timeout)

	transport, ok := hc.Transport.(*http.Transport)
	require.True(t, ok)

	// http2.ConfigureTransports always allocates a TLSClientConfig (to set
	// ALPN protos) even for http:// endpoints.  Verify it is not enabling
	// InsecureSkipVerify on a plain-HTTP endpoint.
	if transport.TLSClientConfig != nil {
		assert.False(t, transport.TLSClientConfig.InsecureSkipVerify, "plain-HTTP endpoint must not skip TLS verification")
	}

	// Stdlib transport timeouts should be set so a single dead conn cannot
	// wedge the client forever.
	assert.True(t, transport.ForceAttemptHTTP2, "ForceAttemptHTTP2 should be true so http2.ConfigureTransports applies")
	assert.Equal(t, 90*time.Second, transport.IdleConnTimeout)
	assert.Equal(t, 10*time.Second, transport.TLSHandshakeTimeout)
	assert.Equal(t, 30*time.Second, transport.ResponseHeaderTimeout)
	assert.Equal(t, 1*time.Second, transport.ExpectContinueTimeout)
}

func TestNewHTTPClient_HTTP2PingsConfigured(t *testing.T) {
	// buildTransport returns the *http2.Transport directly so we can verify
	// the PING knobs without having to dig through unexported fields.
	_, h2 := buildTransport("https://localhost", false)
	require.NotNil(t, h2, "http2.Transport must be configured")
	assert.Equal(t, 15*time.Second, h2.ReadIdleTimeout, "ReadIdleTimeout should send a PING after 15s of no inbound frames")
	assert.Equal(t, 10*time.Second, h2.PingTimeout, "PingTimeout should tear the conn if PING not ACKed in 10s")
	assert.Equal(t, 30*time.Second, h2.WriteByteTimeout, "WriteByteTimeout should catch half-open writes")
}

func TestNewHTTPClient_InsecureHTTPS(t *testing.T) {
	hc := newHTTPClient("https://localhost", true, 10*time.Second)
	transport, ok := hc.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, transport.TLSClientConfig)
	assert.True(t, transport.TLSClientConfig.InsecureSkipVerify)
}

func TestNewHTTPClient_InsecureHTTP_NoTLS(t *testing.T) {
	// Insecure on HTTP should NOT set TLS config.
	hc := newHTTPClient("http://localhost", true, 10*time.Second)
	transport, ok := hc.Transport.(*http.Transport)
	require.True(t, ok)
	if transport.TLSClientConfig != nil {
		assert.False(t, transport.TLSClientConfig.InsecureSkipVerify)
	}
}

func TestNewHTTPClient_DefaultTimeout(t *testing.T) {
	c := NewClient("http://localhost", false, "", "", time.Second, 0)
	_ = c // Just verify no panic; timeout defaulting is in NewClient, not newHTTPClient.
}

func TestNewHTTPClient_TLSConfigType(t *testing.T) {
	hc := newHTTPClient("https://example.com", true, time.Second)
	transport := hc.Transport.(*http.Transport)
	var _ *tls.Config = transport.TLSClientConfig // type assertion
}
