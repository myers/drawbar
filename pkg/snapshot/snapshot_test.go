package snapshot

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	snapshotfake "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned/fake"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestManager() (*Manager, *fake.Clientset, *snapshotfake.Clientset) {
	k8s := fake.NewSimpleClientset()
	snap := snapshotfake.NewSimpleClientset()
	m := &Manager{
		K8sClient:      k8s,
		SnapshotClient: snap,
		Namespace:      "test-ns",
		SnapshotClass:  "openebs-zfs",
		StorageClass:   "openebs-zfs",
		PVCSize:        "10Gi",
		RetentionDays:  7,
	}
	return m, k8s, snap
}

func TestHashCacheKey(t *testing.T) {
	h := HashCacheKey("rust-target-abc123")
	assert.Len(t, h, 32) // 16 bytes = 32 hex chars
	assert.Equal(t, h, HashCacheKey("rust-target-abc123")) // deterministic
	assert.NotEqual(t, h, HashCacheKey("different-key"))
}

func TestFindSnapshot_Miss(t *testing.T) {
	m, _, _ := newTestManager()
	snap, err := m.FindSnapshot(context.Background(), "myorg/myrepo", "some-key")
	require.NoError(t, err)
	assert.Nil(t, snap)
}

func TestFindSnapshot_Hit(t *testing.T) {
	m, _, snapClient := newTestManager()
	ctx := context.Background()

	// Create a snapshot with matching labels.
	_, err := snapClient.SnapshotV1().VolumeSnapshots("test-ns").Create(ctx, &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "snap-1",
			Namespace: "test-ns",
			Labels: map[string]string{
				labelManagedBy:  managerName,
				labelRepository: sanitizeLabelValue("myorg/myrepo"),
				labelCacheKey:   HashCacheKey("my-cache-key"),
			},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	snap, err := m.FindSnapshot(ctx, "myorg/myrepo", "my-cache-key")
	require.NoError(t, err)
	require.NotNil(t, snap)
	assert.Equal(t, "snap-1", snap.Name)
}

func TestCreatePVCFromSnapshot(t *testing.T) {
	m, k8sClient, snapClient := newTestManager()
	ctx := context.Background()

	// Create source snapshot.
	snap, err := snapClient.SnapshotV1().VolumeSnapshots("test-ns").Create(ctx, &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "source-snap", Namespace: "test-ns"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	pvc, err := m.CreatePVCFromSnapshot(ctx, snap, "workspace-42")
	require.NoError(t, err)
	assert.Equal(t, "workspace-42", pvc.Name)
	assert.Equal(t, "VolumeSnapshot", pvc.Spec.DataSource.Kind)
	assert.Equal(t, "source-snap", pvc.Spec.DataSource.Name)

	// Verify it exists in k8s.
	got, err := k8sClient.CoreV1().PersistentVolumeClaims("test-ns").Get(ctx, "workspace-42", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, managerName, got.Labels[labelManagedBy])
}

func TestCreateEmptyPVC(t *testing.T) {
	m, k8sClient, _ := newTestManager()
	ctx := context.Background()

	pvc, err := m.CreateEmptyPVC(ctx, "workspace-empty")
	require.NoError(t, err)
	assert.Equal(t, "workspace-empty", pvc.Name)
	assert.Nil(t, pvc.Spec.DataSource) // no snapshot source

	got, err := k8sClient.CoreV1().PersistentVolumeClaims("test-ns").Get(ctx, "workspace-empty", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, managerName, got.Labels[labelManagedBy])
}

func TestSnapshotPVC(t *testing.T) {
	m, _, snapClient := newTestManager()
	ctx := context.Background()

	snap, err := m.SnapshotPVC(ctx, "workspace-42", "snap-42", "myorg/myrepo", "my-cache-key")
	require.NoError(t, err)
	assert.Equal(t, "snap-42", snap.Name)
	assert.Equal(t, managerName, snap.Labels[labelManagedBy])
	assert.Equal(t, sanitizeLabelValue("myorg/myrepo"), snap.Labels[labelRepository])
	assert.Equal(t, HashCacheKey("my-cache-key"), snap.Labels[labelCacheKey])
	assert.Equal(t, "my-cache-key", snap.Annotations[annotationKeyRaw])

	// Verify in fake client.
	got, err := snapClient.SnapshotV1().VolumeSnapshots("test-ns").Get(ctx, "snap-42", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "workspace-42", *got.Spec.Source.PersistentVolumeClaimName)
}

func TestDeletePVC(t *testing.T) {
	m, k8sClient, _ := newTestManager()
	ctx := context.Background()

	// Create then delete.
	_, err := m.CreateEmptyPVC(ctx, "to-delete")
	require.NoError(t, err)

	err = m.DeletePVC(ctx, "to-delete")
	require.NoError(t, err)

	// Should be gone.
	_, err = k8sClient.CoreV1().PersistentVolumeClaims("test-ns").Get(ctx, "to-delete", metav1.GetOptions{})
	assert.Error(t, err)
}

func TestGarbageCollect(t *testing.T) {
	m, _, snapClient := newTestManager()
	m.RetentionDays = 7
	ctx := context.Background()

	// Create an old snapshot (10 days ago) and a recent one.
	old := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "old-snap",
			Namespace:         "test-ns",
			Labels:            map[string]string{labelManagedBy: managerName},
			CreationTimestamp: metav1.Time{Time: time.Now().AddDate(0, 0, -10)},
		},
	}
	recent := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "recent-snap",
			Namespace:         "test-ns",
			Labels:            map[string]string{labelManagedBy: managerName},
			CreationTimestamp: metav1.Time{Time: time.Now().AddDate(0, 0, -1)},
		},
	}
	_, err := snapClient.SnapshotV1().VolumeSnapshots("test-ns").Create(ctx, old, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = snapClient.SnapshotV1().VolumeSnapshots("test-ns").Create(ctx, recent, metav1.CreateOptions{})
	require.NoError(t, err)

	deleted, err := m.GarbageCollect(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)

	// Recent should still exist.
	_, err = snapClient.SnapshotV1().VolumeSnapshots("test-ns").Get(ctx, "recent-snap", metav1.GetOptions{})
	assert.NoError(t, err)

	// Old should be gone.
	_, err = snapClient.SnapshotV1().VolumeSnapshots("test-ns").Get(ctx, "old-snap", metav1.GetOptions{})
	assert.Error(t, err)
}

func TestSanitizeLabelValue(t *testing.T) {
	assert.Equal(t, "myorg-myrepo", sanitizeLabelValue("myorg/myrepo"))
	assert.Equal(t, "simple", sanitizeLabelValue("simple"))
	assert.Len(t, sanitizeLabelValue("a very long string that exceeds the sixty three character kubernetes label value limit by quite a bit"), 63)
}
