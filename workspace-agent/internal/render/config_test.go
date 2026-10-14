package render

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

func testWorkspace() *v1alpha1.PersonalWorkspace {
	return &v1alpha1.PersonalWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "zhangsan", Namespace: "digital-employees"},
		Spec: v1alpha1.PersonalWorkspaceSpec{
			Runtime: "microvm",
			Resources: v1alpha1.WorkspaceResources{Memory: "1Gi", Disk: "5Gi", VCPUs: 2},
			Companions: []v1alpha1.CompanionSpec{
				{Name: "assistant", Model: "glm-4.6"},
			},
			Credentials: v1alpha1.CredentialsRef{Name: "creds"},
		},
		Status: v1alpha1.PersonalWorkspaceStatus{
			NodeName: "node-a",
			VMSubnet: "192.168.77.0/30",
		},
	}
}

func secret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "digital-employees"}, Data: data}
}

func TestBuildPayloadBasics(t *testing.T) {
	ws := testWorkspace()
	in := Inputs{
		Workspace: ws,
		Manifest:  `{"companions":[{"name":"assistant","memkeySecret":"pw-zhangsan-assistant-memkey"}]}`,
		Memkeys: map[string]*corev1.Secret{
			"assistant": secret("pw-zhangsan-assistant-memkey", map[string][]byte{
				"OPENVIKING_API_KEY":     []byte("ov-key"),
				"WORKSPACE_INTERNAL_TOKEN": []byte("tok"),
			}),
		},
		Credentials:  secret("creds", map[string][]byte{"BIGMODEL_API_KEY": []byte("sk-x")}),
		ModelBaseURL: "http://higress-gateway.higress-system.svc.cluster.local/v1",
		DNSIP:        "10.43.0.10",
		DNSSearch:    "digital-employees.svc.cluster.local",
	}
	p, err := BuildPayload(in)
	require.NoError(t, err)

	env := string(p.EnvFile)
	for _, want := range []string{
		"PORT=8080",
		"WORKSPACE_COMPANIONS_FILE=/etc/workspace/companions.json",
		"WORKSPACE_DATA_DIR=/data",
		"BENCH_BASE_URL=http://higress-gateway.higress-system.svc.cluster.local/v1",
		"BENCH_MODEL=glm-4.6",
		"BENCH_API_KEY=sk-x",
		"PW_GUEST_IP=192.168.77.2",
		"PW_GUEST_GW=192.168.77.1",
		"PW_ASSISTANT_OPENVIKING_API_KEY=ov-key",
		"PW_ASSISTANT_WORKSPACE_INTERNAL_TOKEN=tok",
	} {
		assert.Contains(t, env, want)
	}
	assert.Equal(t, "nameserver 10.43.0.10\nsearch digital-employees.svc.cluster.local\n", string(p.ResolvConf))
	assert.NotEmpty(t, p.Hash)
}

func TestBuildPayloadHashSensitivity(t *testing.T) {
	base := Inputs{
		Workspace:   testWorkspace(),
		Manifest:    `{"companions":[{"name":"assistant"}]}`,
		ModelBaseURL: "http://gw/v1",
		DNSIP:       "10.43.0.10",
	}
	a, err := BuildPayload(base)
	require.NoError(t, err)
	b, err := BuildPayload(base)
	require.NoError(t, err)
	assert.Equal(t, a.Hash, b.Hash, "identical inputs must hash identically")

	changed := base
	changed.Manifest = `{"companions":[{"name":"researcher"}]}`
	c, err := BuildPayload(changed)
	require.NoError(t, err)
	assert.NotEqual(t, a.Hash, c.Hash, "roster change must change the hash")

	// secret rotation must roll the hash too (config reboot semantics)
	rotated := base
	rotated.Memkeys = map[string]*corev1.Secret{"assistant": secret("s", map[string][]byte{"OPENVIKING_API_KEY": []byte("new")})}
	d, err := BuildPayload(rotated)
	require.NoError(t, err)
	assert.NotEqual(t, a.Hash, d.Hash)
}

func TestBuildPayloadRejectsEmptyManifest(t *testing.T) {
	_, err := BuildPayload(Inputs{Workspace: testWorkspace(), DNSIP: "10.43.0.10"})
	assert.ErrorContains(t, err, "manifest")
}

func TestSubnetEndpoints(t *testing.T) {
	h, g, err := subnetEndpoints("192.168.77.4/30")
	require.NoError(t, err)
	assert.Equal(t, "192.168.77.5", h)
	assert.Equal(t, "192.168.77.6", g)
	_, _, err = subnetEndpoints("192.168.77.0/24")
	assert.Error(t, err)
	// host octet 255 boundary
	_, _, err = subnetEndpoints("192.168.0.252/30")
	require.NoError(t, err) // 253/254 fine
}

func TestGuestMACStable(t *testing.T) {
	m1 := GuestMAC("192.168.77.0/30")
	m2 := GuestMAC("192.168.77.0/30")
	assert.Equal(t, m1, m2)
	assert.NotEqual(t, m1, GuestMAC("192.168.77.4/30"))
	assert.True(t, strings.HasPrefix(m1, "06:00:AC:"), "locally administered unicast prefix")
}
