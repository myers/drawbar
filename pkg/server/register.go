package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	pingv1 "code.forgejo.org/forgejo/actions-proto/ping/v1"
	runnerv1 "code.forgejo.org/forgejo/actions-proto/runner/v1"
	"connectrpc.com/connect"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Registration holds the runner's persistent identity.
type Registration struct {
	ID          int64    `json:"id"`
	UUID        string   `json:"uuid"`
	Name        string   `json:"name"`
	Token       string   `json:"token"`
	Address     string   `json:"address"`
	Labels      []string `json:"labels"`
	CacheSecret string   `json:"cache_secret,omitempty"`
}

// CredentialStore abstracts where runner credentials are persisted.
type CredentialStore interface {
	Load(ctx context.Context) (*Registration, error)
	Save(ctx context.Context, reg *Registration) error
}

// RegisterConfig holds everything needed to register or reconnect.
type RegisterConfig struct {
	Endpoint          string
	Insecure          bool
	RegistrationToken string
	Name              string
	Labels            []string
	Version           string
	FetchInterval     time.Duration
	HTTPTimeout       time.Duration
	Store             CredentialStore
}

// EnsureRegistered loads existing credentials or registers anew,
// then calls Declare to update labels/version. Returns a ready Client.
func EnsureRegistered(ctx context.Context, cfg RegisterConfig) (*Client, error) {
	reg, err := cfg.Store.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading credentials: %w", err)
	}

	if reg != nil {
		slog.Info("loaded existing registration",
			"uuid", reg.UUID,
			"name", reg.Name,
			"address", reg.Address,
		)

		client := NewClient(reg.Address, cfg.Insecure, reg.UUID, reg.Token,
			cfg.FetchInterval, cfg.HTTPTimeout)

		if err := declare(ctx, client, cfg.Version, cfg.Labels); err != nil {
			slog.Warn("declare failed, re-registering", "error", err)
			// Fall through to re-register.
		} else {
			return client, nil
		}
	}

	if cfg.RegistrationToken == "" {
		return nil, fmt.Errorf("no existing registration and no registration token provided")
	}

	return register(ctx, cfg)
}

func register(ctx context.Context, cfg RegisterConfig) (*Client, error) {
	// Create an unauthenticated client for Ping + Register.
	client := NewClient(cfg.Endpoint, cfg.Insecure, "", "", cfg.FetchInterval, cfg.HTTPTimeout)

	// Ping to verify connectivity.
	_, err := client.Ping(ctx, connect.NewRequest(&pingv1.PingRequest{
		Data: "ping",
	}))
	if err != nil {
		return nil, fmt.Errorf("ping failed (is %s reachable?): %w", cfg.Endpoint, err)
	}
	slog.Info("ping successful", "endpoint", cfg.Endpoint)

	// Register.
	resp, err := client.Register(ctx, connect.NewRequest(&runnerv1.RegisterRequest{
		Name:    cfg.Name,
		Token:   cfg.RegistrationToken,
		Version: cfg.Version,
		Labels:  cfg.Labels,
	}))
	if err != nil {
		return nil, fmt.Errorf("registration failed: %w", err)
	}

	runner := resp.Msg.GetRunner()
	reg := &Registration{
		ID:      runner.GetId(),
		UUID:    runner.GetUuid(),
		Name:    runner.GetName(),
		Token:   runner.GetToken(),
		Address: cfg.Endpoint,
		Labels:  cfg.Labels,
	}

	slog.Info("registered successfully",
		"id", reg.ID,
		"uuid", reg.UUID,
		"name", reg.Name,
	)

	if err := cfg.Store.Save(ctx, reg); err != nil {
		return nil, fmt.Errorf("saving credentials: %w", err)
	}

	// Create an authenticated client.
	authedClient := NewClient(cfg.Endpoint, cfg.Insecure, reg.UUID, reg.Token,
		cfg.FetchInterval, cfg.HTTPTimeout)

	if err := declare(ctx, authedClient, cfg.Version, cfg.Labels); err != nil {
		// Declare is best-effort — older Forgejo versions don't support it.
		slog.Warn("declare failed (may be unsupported)", "error", err)
	}

	return authedClient, nil
}

func declare(ctx context.Context, client *Client, version string, labels []string) error {
	_, err := client.Declare(ctx, connect.NewRequest(&runnerv1.DeclareRequest{
		Version: version,
		Labels:  labels,
	}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			slog.Info("declare not supported by server (older Forgejo version)")
			return nil
		}
		return err
	}
	slog.Info("declared labels and version")
	return nil
}

// FileStore stores credentials in a local JSON file.
type FileStore struct {
	Path string
}

func (f *FileStore) Load(_ context.Context) (*Registration, error) {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var reg Registration
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parsing credential file: %w", err)
	}
	return &reg, nil
}

func (f *FileStore) Save(_ context.Context, reg *Registration) error {
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(f.Path, data, 0o600)
}

// SecretStore stores credentials in a Kubernetes Secret.
type SecretStore struct {
	Client    kubernetes.Interface
	Namespace string
	Name      string
}

const secretDataKey = "registration"

func (s *SecretStore) Load(ctx context.Context) (*Registration, error) {
	secret, err := s.Client.CoreV1().Secrets(s.Namespace).Get(ctx, s.Name, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting secret %s/%s: %w", s.Namespace, s.Name, err)
	}

	data, ok := secret.Data[secretDataKey]
	if !ok {
		return nil, nil
	}

	var reg Registration
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parsing secret data: %w", err)
	}
	return &reg, nil
}

func (s *SecretStore) Save(ctx context.Context, reg *Registration) error {
	data, err := json.Marshal(reg)
	if err != nil {
		return err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.Name,
			Namespace: s.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "drawbar",
			},
		},
		Data: map[string][]byte{
			secretDataKey: data,
		},
	}

	// Try create first, update if it already exists.
	_, err = s.Client.CoreV1().Secrets(s.Namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		if k8serrors.IsAlreadyExists(err) {
			_, err = s.Client.CoreV1().Secrets(s.Namespace).Update(ctx, secret, metav1.UpdateOptions{})
		}
	}
	if err != nil {
		return fmt.Errorf("saving secret %s/%s: %w", s.Namespace, s.Name, err)
	}

	slog.Info("credentials saved to k8s secret", "namespace", s.Namespace, "name", s.Name)
	return nil
}
