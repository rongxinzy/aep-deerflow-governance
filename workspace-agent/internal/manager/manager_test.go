package manager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bytedance/mockey"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/disk"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/hostcmd"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/vm"
	vm_mocks "github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/vm/mocks"
)

const manifestV1 = `{"owner":"u1","workspace":"ws1","companions":[{"name":"assistant","displayName":"助手","model":"glm-4.6","memkeySecret":"pw-ws1-assistant-memkey"}]}`

const manifestV2 = `{"owner":"u1","workspace":"ws1","companions":[{"name":"assistant","displayName":"助手","model":"glm-4.6","memkeySecret":"pw-ws1-assistant-memkey"},{"name":"researcher","model":"glm-4.6","memkeySecret":"pw-ws1-researcher-memkey"}]}`

type env struct {
	lastConfig vm.Config
	m          *Manager
	ctrl       *gomock.Controller
	driver     *vm_mocks.MockDriver
	client     client.Client
	// mutable driver/healthz state
	alive    bool
	healthy  bool
	startErr error
	starts   int
	stops    int
}

func newEnv(t *testing.T, objects ...client.Object) *env {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PersonalWorkspace{}).
		WithObjects(objects...).Build()

	workdir := t.TempDir()
	// fake template (contents irrelevant; sums are over the content)
	writeFakeTemplate(t, filepath.Join(workdir, "templates", "0.5.0"))

	ctrl := gomock.NewController(t)
	e := &env{client: c, ctrl: ctrl, healthy: true}
	e.driver = vm_mocks.NewMockDriver(ctrl)
	e.driver.EXPECT().Alive().DoAndReturn(func() bool { return e.alive }).AnyTimes()
	e.driver.EXPECT().Start(gomock.Any()).DoAndReturn(func(config vm.Config) error {
		e.lastConfig = config
		if e.startErr != nil {
			return e.startErr
		}
		e.alive = true
		e.starts++
		return nil
	}).AnyTimes()
	e.driver.EXPECT().Stop(gomock.Any()).DoAndReturn(func(time.Duration) error {
		e.alive = false
		e.stops++
		return nil
	}).AnyTimes()

	runner := &hostcmd.FakeRunner{}
	e.m = New(Config{
		Namespace: "digital-employees", NodeName: "node-a", HostIP: "10.1.2.3",
		Workdir: workdir, TemplateTag: "0.5.0", PortLo: 8800, PortHi: 8899,
		ModelBaseURL: "http://gw/v1", SerialLogMB: 5,
	}, c, runner, &disk.Images{Run: runner}, func(string) vm.Driver { return e.driver })

	// healthz probes are patched (network to a fake guest IP)
	un1 := mockey.Mock(probeHealthz).To(func(string, time.Duration) bool { return e.healthy }).Build()
	un2 := mockey.Mock(waitForHealthz).To(func(string, time.Duration) bool { return e.healthy }).Build()
	un3 := mockey.Mock(prepareGuestStop).To(func(string, string) {}).Build()
	t.Cleanup(func() { un1.UnPatch(); un2.UnPatch(); un3.UnPatch() })
	return e
}

func writeFakeTemplate(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	sums := ""
	for _, name := range []string{"rootfs.ext4", "vmlinux", "firecracker"} {
		content := []byte("x-" + name)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), content, 0o755))
		sum := sha256.Sum256(content)
		sums += hex.EncodeToString(sum[:]) + "  " + name + "\n"
	}
	metadata, err := json.Marshal(map[string]any{"tag": filepath.Base(dir), "desktop": true})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "template.json"), metadata, 0644))
	sum := sha256.Sum256(metadata)
	sums += hex.EncodeToString(sum[:]) + "  template.json\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums), 0o644))
}

func workspaceCR() *v1alpha1.PersonalWorkspace {
	return &v1alpha1.PersonalWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws1", Namespace: "digital-employees"},
		Spec: v1alpha1.PersonalWorkspaceSpec{
			Owner: "u1", Runtime: "microvm",
			Resources:   v1alpha1.WorkspaceResources{Memory: "1Gi", Disk: "5Gi", VCPUs: 2},
			Companions:  []v1alpha1.CompanionSpec{{Name: "assistant", Model: "glm-4.6"}},
			Credentials: v1alpha1.CredentialsRef{Name: "creds"},
		},
		Status: v1alpha1.PersonalWorkspaceStatus{
			NodeName: "node-a", VMSubnet: "192.168.77.0/30",
			Phase: v1alpha1.WorkspacePhasePending,
		},
	}
}

func baseObjects(manifest string) []client.Object {
	return []client.Object{
		workspaceCR(),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "pw-ws1-companions", Namespace: "digital-employees"},
			Data: map[string]string{"companions.json": manifest}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pw-ws1-assistant-memkey", Namespace: "digital-employees"},
			Data: map[string][]byte{"OPENVIKING_API_KEY": []byte("ovk"), "WORKSPACE_INTERNAL_TOKEN": []byte("tok")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pw-ws1-researcher-memkey", Namespace: "digital-employees"},
			Data: map[string][]byte{"OPENVIKING_API_KEY": []byte("ovk2"), "WORKSPACE_INTERNAL_TOKEN": []byte("tok2")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "digital-employees"},
			Data: map[string][]byte{"BIGMODEL_API_KEY": []byte("sk")}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kube-dns"}, Spec: corev1.ServiceSpec{ClusterIP: "10.43.0.10"}},
		&corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kube-dns"}, Subsets: []corev1.EndpointSubset{{Addresses: []corev1.EndpointAddress{{IP: "10.42.0.53"}}}}},
		&corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "higress-gateway", Namespace: "higress-system"},
			Subsets: []corev1.EndpointSubset{{Addresses: []corev1.EndpointAddress{{IP: "10.42.0.5"}}}}},
	}
}

func getWS(t *testing.T, c client.Client) *v1alpha1.PersonalWorkspace {
	ws := &v1alpha1.PersonalWorkspace{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "digital-employees", Name: "ws1"}, ws))
	return ws
}

func TestBootToRunning(t *testing.T) {
	e := newEnv(t, baseObjects(manifestV1)...)
	require.NoError(t, e.m.Resync(context.Background()))

	ws := getWS(t, e.client)
	assert.Equal(t, v1alpha1.WorkspacePhaseRunning, ws.Status.Phase)
	assert.Equal(t, "10.1.2.3:8800", ws.Status.GatewayEndpoint)
	require.NotNil(t, ws.Status.VM)
	assert.Equal(t, int32(8800), ws.Status.VM.HostPort)
	assert.NotEmpty(t, ws.Status.VM.ConfigHash)
	assert.Contains(t, ws.Finalizers, AgentFinalizer)
	assert.Equal(t, 1, e.starts)
	// disks materialized
	assert.FileExists(t, filepath.Join(e.m.cfg.Workdir, "disks", "ws1", "data.ext4"))
	assert.FileExists(t, filepath.Join(e.m.cfg.Workdir, "disks", "ws1", "config.ext4"))
}

func TestConfigChangeReboots(t *testing.T) {
	e := newEnv(t, baseObjects(manifestV1)...)
	require.NoError(t, e.m.Resync(context.Background()))
	firstHash := getWS(t, e.client).Status.VM.ConfigHash

	cm := &corev1.ConfigMap{}
	require.NoError(t, e.client.Get(context.Background(), client.ObjectKey{Namespace: "digital-employees", Name: "pw-ws1-companions"}, cm))
	cm.Data["companions.json"] = manifestV2
	require.NoError(t, e.client.Update(context.Background(), cm))

	require.NoError(t, e.m.Resync(context.Background()))
	assert.Equal(t, 1, e.stops, "config change stops the VM before rewriting the config disk")
	assert.Equal(t, 2, e.starts)
	ws := getWS(t, e.client)
	assert.Equal(t, v1alpha1.WorkspacePhaseRunning, ws.Status.Phase)
	assert.NotEqual(t, firstHash, ws.Status.VM.ConfigHash)
}

func TestDeleteTearsDownAndReleasesFinalizer(t *testing.T) {
	e := newEnv(t, baseObjects(manifestV1)...)
	require.NoError(t, e.m.Resync(context.Background()))

	ws := getWS(t, e.client)
	require.NoError(t, e.client.Delete(context.Background(), ws))
	require.NoError(t, e.m.Resync(context.Background()))

	assert.Equal(t, 1, e.stops)
	gone := &v1alpha1.PersonalWorkspace{}
	err := e.client.Get(context.Background(), client.ObjectKey{Namespace: "digital-employees", Name: "ws1"}, gone)
	assert.Error(t, err, "finalizer released so the CR is actually deleted")
	assert.NoDirExists(t, filepath.Join(e.m.cfg.Workdir, "disks", "ws1"))
	assert.NoDirExists(t, filepath.Join(e.m.cfg.Workdir, "run", "ws1"))
}

func TestBootGiveUpAfterFiveFails(t *testing.T) {
	e := newEnv(t, baseObjects(manifestV1)...)
	e.startErr = assert.AnError
	// First reconcile records failure #1.
	_ = e.m.Resync(context.Background())
	// Force the counter to the edge (time-based backoff is unrolled here).
	e.m.mu.Lock()
	if s := e.m.supers["ws1"]; s != nil {
		s.bootFails = 4
		s.lastBoot = time.Now().Add(-time.Minute)
	}
	e.m.mu.Unlock()
	// Second reconcile: backoff elapsed, Start fails again (#5).
	_ = e.m.Resync(context.Background())
	// Third reconcile: refuses to boot, workspace Failed (the give-up state
	// surfaces as the workspace error until spec/config changes).
	err := e.m.Resync(context.Background())
	require.ErrorContains(t, err, "giving up")
	ws := getWS(t, e.client)
	assert.Equal(t, v1alpha1.WorkspacePhaseFailed, ws.Status.Phase)
	assert.Contains(t, ws.Status.Message, "giving up")
}

func TestRecordedPortIsSticky(t *testing.T) {
	objects := baseObjects(manifestV1)
	ws := objects[0].(*v1alpha1.PersonalWorkspace)
	ws.Status.VM = &v1alpha1.WorkspaceVMStatus{HostPort: 8855}
	e := newEnv(t, objects...)
	require.NoError(t, e.m.Resync(context.Background()))
	fresh := getWS(t, e.client)
	require.NotNil(t, fresh.Status.VM)
	assert.Equal(t, int32(8855), fresh.Status.VM.HostPort)
	assert.Equal(t, "10.1.2.3:8855", fresh.Status.GatewayEndpoint)
}

func TestIgnoresForeignAndUnassigned(t *testing.T) {
	objects := baseObjects(manifestV1)
	ws := objects[0].(*v1alpha1.PersonalWorkspace)
	ws.Status.NodeName = "node-b" // assigned elsewhere
	pod := &v1alpha1.PersonalWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws2", Namespace: "digital-employees"},
		Spec:       v1alpha1.PersonalWorkspaceSpec{Runtime: "pod"},
		Status:     v1alpha1.PersonalWorkspaceStatus{NodeName: "node-a"},
	}
	objects = append(objects, pod)
	e := newEnv(t, objects...)
	require.NoError(t, e.m.Resync(context.Background()))
	assert.Equal(t, 0, e.starts, "neither a foreign-assigned microvm nor a pod-runtime workspace boots")
}

func TestDesktopResourcesAndTemplateRolloutKeepsData(t *testing.T) {
	e := newEnv(t, baseObjects(manifestV1)...)
	require.NoError(t, e.m.Resync(context.Background()))
	data := filepath.Join(e.m.cfg.Workdir, "disks", "ws1", "data.ext4")
	require.NoError(t, os.WriteFile(data, []byte("persistent-user-content"), 0600))
	ws := getWS(t, e.client)
	ws.Spec.Desktop.Enabled = true
	ws.Spec.Resources.Memory = "4Gi"
	ws.Spec.Resources.VCPUs = 4
	ws.Spec.TemplateTag = "desktop-v1"
	writeFakeTemplate(t, filepath.Join(e.m.cfg.Workdir, "templates", "desktop-v1"))
	require.NoError(t, e.client.Update(context.Background(), ws))
	require.NoError(t, e.m.Resync(context.Background()))
	require.Equal(t, 2, e.starts)
	require.Equal(t, int32(4096), e.lastConfig.MemMib)
	require.Equal(t, int32(4), e.lastConfig.VCPUs)
	require.Contains(t, e.lastConfig.Rootfs, "desktop-v1")
	got := getWS(t, e.client)
	require.Equal(t, "desktop-v1", got.Status.VM.TemplateTag)
	require.True(t, got.Status.VM.DesktopEnabled)
	require.Equal(t, int32(4), got.Status.VM.Resources.VCPUs)
	content, err := os.ReadFile(data)
	require.NoError(t, err)
	require.Equal(t, "persistent-user-content", string(content))
	require.NoError(t, e.m.Resync(context.Background()))
	require.Equal(t, 2, e.starts)
	// Missing template is rejected before stopping the healthy guest.
	ws = getWS(t, e.client)
	ws.Spec.TemplateTag = "missing"
	require.NoError(t, e.client.Update(context.Background(), ws))
	require.Error(t, e.m.Resync(context.Background()))
	require.Equal(t, 1, e.stops)
}

func TestEarlyProvisioningFailureIsVisibleAndRecovers(t *testing.T) {
	objects := baseObjects(manifestV1)
	// The roster has not arrived yet: failure occurs before a VM exists.
	roster := objects[1]
	e := newEnv(t, append(objects[:1:1], objects[2:]...)...)
	require.Error(t, e.m.Resync(context.Background()))
	ws := getWS(t, e.client)
	assert.Equal(t, v1alpha1.WorkspacePhaseFailed, ws.Status.Phase)
	assert.Contains(t, ws.Status.Message, "自动重试")
	require.NoError(t, e.client.Create(context.Background(), roster))
	require.NoError(t, e.m.Resync(context.Background()))
	ws = getWS(t, e.client)
	assert.Equal(t, v1alpha1.WorkspacePhaseRunning, ws.Status.Phase)
	assert.Empty(t, ws.Status.Message)
}
