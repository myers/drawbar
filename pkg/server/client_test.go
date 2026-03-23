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
	assert.Nil(t, transport.TLSClientConfig)
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
	// Should be nil or have InsecureSkipVerify=false.
	if transport.TLSClientConfig != nil {
		assert.False(t, transport.TLSClientConfig.InsecureSkipVerify)
	}
}

func TestNewHTTPClient_DefaultTimeout(t *testing.T) {
	// When httpTimeout is 0, NewClient sets it to 60s before calling newHTTPClient.
	c := NewClient("http://localhost", false, "", "", time.Second, 0)
	_ = c // Just verify no panic; timeout defaulting is in NewClient, not newHTTPClient.
}

// Verify TLS types are as expected.
func TestNewHTTPClient_TLSConfigType(t *testing.T) {
	hc := newHTTPClient("https://example.com", true, time.Second)
	transport := hc.Transport.(*http.Transport)
	var _ *tls.Config = transport.TLSClientConfig // type assertion
}
