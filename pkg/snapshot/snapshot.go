// Package snapshot manages ZFS-backed workspace caching via k8s VolumeSnapshot CRD.
// When enabled, workspace PVCs are created from snapshots (instant ZFS clone) instead
// of empty directories, making cache restore O(1) for large build artifacts.
package snapshot

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	snapshotclient "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

const (
	labelManagedBy  = "app.kubernetes.io/managed-by"
	labelRepository = "drawbar.dev/repository"
	labelCacheKey   = "drawbar.dev/cache-key"
	annotationKeyRaw = "drawbar.dev/cache-key-raw"
	managerName     = "drawbar"
)

// Manager handles workspace snapshot lifecycle.
type Manager struct {
	K8sClient      kubernetes.Interface
	SnapshotClient snapshotclient.Interface
	Namespace      string
	SnapshotClass  string // VolumeSnapshotClass name
	StorageClass   string // StorageClass for PVCs created from snapshots
	PVCSize        string // e.g., "10Gi"
	RetentionDays  int    // snapshots older than this are GC'd
}

// HashCacheKey returns a k8s-label-safe hash of a cache key string.
// Labels are limited to 63 chars; we use a truncated SHA256.
func HashCacheKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", h[:16]) // 32 hex chars
}

// FindSnapshot looks up the most recent VolumeSnapshot matching a repo + cache key.
func (m *Manager) FindSnapshot(ctx context.Context, repo, cacheKey string) (*snapshotv1.VolumeSnapshot, error) {
	selector := fmt.Sprintf("%s=%s,%s=%s,%s=%s",
		labelManagedBy, managerName,
		labelRepository, sanitizeLabelValue(repo),
		labelCacheKey, HashCacheKey(cacheKey),
	)

	list, err := m.SnapshotClient.SnapshotV1().VolumeSnapshots(m.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("listing snapshots: %w", err)
	}

	if len(list.Items) == 0 {
		return nil, nil // cache miss
	}

	// Return the most recently created snapshot.
	latest := &list.Items[0]
	for i := range list.Items {
		if list.Items[i].CreationTimestamp.After(latest.CreationTimestamp.Time) {
			latest = &list.Items[i]
		}
	}

	slog.Info("found cached snapshot", "snapshot", latest.Name, "repo", repo, "cache_key", cacheKey)
	return latest, nil
}

// CreatePVCFromSnapshot creates a PVC backed by a VolumeSnapshot (ZFS clone).
func (m *Manager) CreatePVCFromSnapshot(ctx context.Context, snapshot *snapshotv1.VolumeSnapshot, pvcName string) (*corev1.PersistentVolumeClaim, error) {
	size := resource.MustParse(m.PVCSize)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: m.Namespace,
			Labels: map[string]string{
				labelManagedBy: managerName,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: ptr.To("snapshot.storage.k8s.io"),
				Kind:     "VolumeSnapshot",
				Name:     snapshot.Name,
			},
			StorageClassName: &m.StorageClass,
		},
	}

	created, err := m.K8sClient.CoreV1().PersistentVolumeClaims(m.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating PVC from snapshot %s: %w", snapshot.Name, err)
	}
	slog.Info("created workspace PVC from snapshot", "pvc", pvcName, "snapshot", snapshot.Name)
	return created, nil
}

// CreateEmptyPVC creates a fresh PVC for a cache miss.
func (m *Manager) CreateEmptyPVC(ctx context.Context, pvcName string) (*corev1.PersistentVolumeClaim, error) {
	size := resource.MustParse(m.PVCSize)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: m.Namespace,
			Labels: map[string]string{
				labelManagedBy: managerName,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
			StorageClassName: &m.StorageClass,
		},
	}

	created, err := m.K8sClient.CoreV1().PersistentVolumeClaims(m.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating empty PVC: %w", err)
	}
	slog.Info("created empty workspace PVC (cache miss)", "pvc", pvcName)
	return created, nil
}

// SnapshotPVC creates a VolumeSnapshot of a workspace PVC and labels it for lookup.
func (m *Manager) SnapshotPVC(ctx context.Context, pvcName, snapshotName, repo, cacheKey string) (*snapshotv1.VolumeSnapshot, error) {
	snap := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      snapshotName,
			Namespace: m.Namespace,
			Labels: map[string]string{
				labelManagedBy:  managerName,
				labelRepository: sanitizeLabelValue(repo),
				labelCacheKey:   HashCacheKey(cacheKey),
			},
			Annotations: map[string]string{
				annotationKeyRaw: cacheKey,
			},
		},
		Spec: snapshotv1.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &m.SnapshotClass,
			Source: snapshotv1.VolumeSnapshotSource{
				PersistentVolumeClaimName: &pvcName,
			},
		},
	}

	created, err := m.SnapshotClient.SnapshotV1().VolumeSnapshots(m.Namespace).Create(ctx, snap, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating snapshot of %s: %w", pvcName, err)
	}
	slog.Info("created workspace snapshot", "snapshot", snapshotName, "pvc", pvcName, "repo", repo)
	return created, nil
}

// DeletePVC deletes a workspace PVC.
func (m *Manager) DeletePVC(ctx context.Context, pvcName string) error {
	err := m.K8sClient.CoreV1().PersistentVolumeClaims(m.Namespace).Delete(ctx, pvcName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("deleting PVC %s: %w", pvcName, err)
	}
	return nil
}

// GarbageCollect deletes snapshots older than the retention period.
// Returns the number of snapshots deleted.
func (m *Manager) GarbageCollect(ctx context.Context) (int, error) {
	selector := fmt.Sprintf("%s=%s", labelManagedBy, managerName)
	list, err := m.SnapshotClient.SnapshotV1().VolumeSnapshots(m.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return 0, fmt.Errorf("listing snapshots for GC: %w", err)
	}

	cutoff := time.Now().AddDate(0, 0, -m.RetentionDays)
	deleted := 0
	for _, snap := range list.Items {
		if snap.CreationTimestamp.Time.Before(cutoff) {
			err := m.SnapshotClient.SnapshotV1().VolumeSnapshots(m.Namespace).Delete(ctx, snap.Name, metav1.DeleteOptions{})
			if err != nil {
				slog.Warn("failed to delete expired snapshot", "snapshot", snap.Name, "error", err)
				continue
			}
			slog.Info("deleted expired snapshot", "snapshot", snap.Name, "age_days", time.Since(snap.CreationTimestamp.Time).Hours()/24)
			deleted++
		}
	}
	return deleted, nil
}

// sanitizeLabelValue makes a string safe for k8s label values (max 63 chars, alphanumeric + dash/dot/underscore).
func sanitizeLabelValue(s string) string {
	if len(s) > 63 {
		s = s[:63]
	}
	var b []byte
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			b = append(b, c)
		} else {
			b = append(b, '-')
		}
	}
	return string(b)
}
