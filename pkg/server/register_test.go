package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pingv1 "code.gitea.io/actions-proto-go/ping/v1"
	runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
	"code.gitea.io/actions-proto-go/runner/v1/runnerv1connect"
	"connectrpc.com/connect"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// --- FileStore ---

func TestFileStore_SaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.json")
	store := &FileStore{Path: path}
	ctx := context.Background()

	reg := &Registration{
		ID:      42,
		UUID:    "test-uuid",
		Name:    "test-runner",
		Token:   "secret-token",
		Address: "https://gitea.example.com",
		Labels:  []string{"ubuntu-latest"},
	}

	require.NoError(t, store.Save(ctx, reg))

	loaded, err := store.Load(ctx)
	require.NoError(t, err)
	require.NotNil(t, loaded)

	assert.Equal(t, reg.ID, loaded.ID)
	assert.Equal(t, reg.UUID, loaded.UUID)
	assert.Equal(t, reg.Token, loaded.Token)
	assert.Equal(t, reg.Labels, loaded.Labels)
}

func TestFileStore_Load_NotExist(t *testing.T) {
	store := &FileStore{Path: filepath.Join(t.TempDir(), "missing.json")}
	reg, err := store.Load(context.Background())
	require.NoError(t, err)
	assert.Nil(t, reg)
}

func TestFileStore_Load_InvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))

	store := &FileStore{Path: path}
	_, err := store.Load(context.Background())
	assert.Error(t, err)
}

func TestFileStore_Save_Permissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.json")
	store := &FileStore{Path: path}
	require.NoError(t, store.Save(context.Background(), &Registration{ID: 1}))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// --- SecretStore ---

func TestSecretStore_SaveAndLoad(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := &SecretStore{Client: client, Namespace: "ns", Name: "runner-creds"}
	ctx := context.Background()

	reg := &Registration{ID: 1, UUID: "uuid-1", Token: "tok"}
	require.NoError(t, store.Save(ctx, reg))

	loaded, err := store.Load(ctx)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, reg.UUID, loaded.UUID)
	assert.Equal(t, reg.Token, loaded.Token)
}

func TestSecretStore_Load_NotFound(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := &SecretStore{Client: client, Namespace: "ns", Name: "missing"}

	reg, err := store.Load(context.Background())
	require.NoError(t, err)
	assert.Nil(t, reg)
}

func TestSecretStore_Load_MissingDataKey(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "runner-creds", Namespace: "ns"},
		Data:       map[string][]byte{"other-key": []byte("data")},
	}
	client := fake.NewSimpleClientset(secret)
	store := &SecretStore{Client: client, Namespace: "ns", Name: "runner-creds"}

	reg, err := store.Load(context.Background())
	require.NoError(t, err)
	assert.Nil(t, reg)
}

func TestSecretStore_Save_CreateThenUpdate(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := &SecretStore{Client: client, Namespace: "ns", Name: "runner-creds"}
	ctx := context.Background()

	// First save creates.
	require.NoError(t, store.Save(ctx, &Registration{ID: 1, UUID: "v1"}))
	loaded, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, "v1", loaded.UUID)

	// Second save updates.
	require.NoError(t, store.Save(ctx, &Registration{ID: 1, UUID: "v2"}))
	loaded, err = store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, "v2", loaded.UUID)
}

func TestSecretStore_Save_Labels(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := &SecretStore{Client: client, Namespace: "ns", Name: "runner-creds"}
	ctx := context.Background()

	require.NoError(t, store.Save(ctx, &Registration{ID: 1}))

	secret, err := client.CoreV1().Secrets("ns").Get(ctx, "runner-creds", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "drawbar", secret.Labels["app.kubernetes.io/managed-by"])
}

// --- declare (with connect test server) ---

type declareHandler struct {
	err error
}

func (h *declareHandler) Declare(_ context.Context, _ *connect.Request[runnerv1.DeclareRequest]) (*connect.Response[runnerv1.DeclareResponse], error) {
	if h.err != nil {
		return nil, h.err
	}
	return connect.NewResponse(&runnerv1.DeclareResponse{}), nil
}

func newTestClientWithDeclare(t *testing.T, handler *declareHandler) *Client {
	t.Helper()
	mux := http.NewServeMux()
	// The client prepends /api/actions to the base URL, so register under that prefix.
	prefix := "/api/actions"
	mux.Handle(prefix+runnerv1connect.RunnerServiceDeclareProcedure, connect.NewUnaryHandler(
		runnerv1connect.RunnerServiceDeclareProcedure,
		handler.Declare,
	))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return NewClient(server.URL, false, "uuid", "token", time.Second, 5*time.Second)
}

func TestDeclare_Success(t *testing.T) {
	client := newTestClientWithDeclare(t, &declareHandler{})
	err := declare(context.Background(), client, "1.0", []string{"ubuntu"})
	assert.NoError(t, err)
}

func TestDeclare_Unimplemented_NoError(t *testing.T) {
	client := newTestClientWithDeclare(t, &declareHandler{
		err: connect.NewError(connect.CodeUnimplemented, nil),
	})
	err := declare(context.Background(), client, "1.0", []string{"ubuntu"})
	assert.NoError(t, err)
}

func TestDeclare_OtherError(t *testing.T) {
	client := newTestClientWithDeclare(t, &declareHandler{
		err: connect.NewError(connect.CodeInternal, nil),
	})
	err := declare(context.Background(), client, "1.0", []string{"ubuntu"})
	assert.Error(t, err)
}

// --- EnsureRegistered / register ---

type fullHandler struct {
	pingErr     error
	registerErr error
	declareErr  error
	runnerResp  *runnerv1.Runner
}

func (h *fullHandler) serveMux(prefix string) *http.ServeMux {
	mux := http.NewServeMux()

	mux.Handle(prefix+"/ping.v1.PingService/Ping", connect.NewUnaryHandler(
		"/ping.v1.PingService/Ping",
		func(_ context.Context, _ *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
			if h.pingErr != nil {
				return nil, h.pingErr
			}
			return connect.NewResponse(&pingv1.PingResponse{Data: "pong"}), nil
		},
	))
	mux.Handle(prefix+runnerv1connect.RunnerServiceRegisterProcedure, connect.NewUnaryHandler(
		runnerv1connect.RunnerServiceRegisterProcedure,
		func(_ context.Context, _ *connect.Request[runnerv1.RegisterRequest]) (*connect.Response[runnerv1.RegisterResponse], error) {
			if h.registerErr != nil {
				return nil, h.registerErr
			}
			runner := h.runnerResp
			if runner == nil {
				runner = &runnerv1.Runner{Id: 1, Uuid: "uuid-1", Name: "runner", Token: "tok-1"}
			}
			return connect.NewResponse(&runnerv1.RegisterResponse{Runner: runner}), nil
		},
	))
	mux.Handle(prefix+runnerv1connect.RunnerServiceDeclareProcedure, connect.NewUnaryHandler(
		runnerv1connect.RunnerServiceDeclareProcedure,
		func(_ context.Context, _ *connect.Request[runnerv1.DeclareRequest]) (*connect.Response[runnerv1.DeclareResponse], error) {
			if h.declareErr != nil {
				return nil, h.declareErr
			}
			return connect.NewResponse(&runnerv1.DeclareResponse{}), nil
		},
	))
	return mux
}

func TestEnsureRegistered_ExistingCredentials(t *testing.T) {
	h := &fullHandler{}
	server := httptest.NewServer(h.serveMux("/api/actions"))
	t.Cleanup(server.Close)

	store := &FileStore{Path: filepath.Join(t.TempDir(), "creds.json")}
	ctx := context.Background()
	// Pre-save credentials.
	require.NoError(t, store.Save(ctx, &Registration{
		ID: 1, UUID: "uuid-1", Name: "runner", Token: "tok-1", Address: server.URL,
	}))

	client, err := EnsureRegistered(ctx, RegisterConfig{
		Endpoint: server.URL,
		Name:     "runner",
		Labels:   []string{"ubuntu"},
		Version:  "1.0",
		Store:    store,
		FetchInterval: time.Second,
		HTTPTimeout:   5 * time.Second,
	})
	require.NoError(t, err)
	assert.NotNil(t, client)
}

func TestEnsureRegistered_FreshRegistration(t *testing.T) {
	h := &fullHandler{}
	server := httptest.NewServer(h.serveMux("/api/actions"))
	t.Cleanup(server.Close)

	store := &FileStore{Path: filepath.Join(t.TempDir(), "creds.json")}
	ctx := context.Background()

	client, err := EnsureRegistered(ctx, RegisterConfig{
		Endpoint:          server.URL,
		RegistrationToken: "reg-token",
		Name:              "runner",
		Labels:            []string{"ubuntu"},
		Version:           "1.0",
		Store:             store,
		FetchInterval:     time.Second,
		HTTPTimeout:       5 * time.Second,
	})
	require.NoError(t, err)
	assert.NotNil(t, client)

	// Credentials should be persisted.
	loaded, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, "uuid-1", loaded.UUID)
}

func TestEnsureRegistered_NoCredentialsNoToken(t *testing.T) {
	store := &FileStore{Path: filepath.Join(t.TempDir(), "creds.json")}
	_, err := EnsureRegistered(context.Background(), RegisterConfig{
		Endpoint: "http://localhost",
		Store:    store,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no existing registration")
}
