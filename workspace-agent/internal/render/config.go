// Package render builds the guest config-disk payload for one microvm
// workspace: the companions manifest (copied verbatim from the operator's
// ConfigMap — single roster source), the env file the guest init sources,
// and the resolv.conf pointing at kube-dns. The payload hash is the config
// rollout identity: a change reboots the VM.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// Guest networking facts the init script consumes (static config; the VM
// has no DHCP).
const (
	envPort          = "PORT=8080"
	envCompanionsKey = "WORKSPACE_COMPANIONS_FILE"
	envCompanionsVal = "/etc/workspace/companions.json"
	envDataDir       = "WORKSPACE_DATA_DIR=/data"
	envChildPortBase = "WORKSPACE_CHILD_PORT_BASE=8101"
	envIdleMS        = "WORKSPACE_IDLE_MS=300000"
)

// ConfigPayload is the full config-disk content set plus its hash.
type ConfigPayload struct {
	// CompanionsJSON is copied byte-for-byte from the ConfigMap.
	CompanionsJSON []byte
	// EnvFile is the KEY=VAL file the guest init sources.
	EnvFile []byte
	// ResolvConf points the guest at kube-dns.
	ResolvConf []byte
	// Hash identifies the payload (hex sha256 over all three parts).
	Hash string
}

// Inputs are everything BuildPayload needs from the API server.
type Inputs struct {
	Workspace      *v1alpha1.PersonalWorkspace
	LifecycleToken string
	// Manifest is the companions.json ConfigMap data.
	Manifest string
	// Memkeys maps companion name -> memkey Secret.
	Memkeys map[string]*corev1.Secret
	// Credentials is the platform credentials secret (model API key).
	Credentials *corev1.Secret
	// ModelBaseURL is the model gateway route (service DNS form).
	ModelBaseURL string
	// MemoryBaseURL is the OpenViking service the companions dial directly
	// (rendered as global OPENVIKING_BASE_URL; empty skips the line).
	MemoryBaseURL string
	// DNSIP is the kube-dns service ClusterIP.
	DNSIP string
	// DNSSearch is the resolv.conf search line (optional).
	DNSSearch string
}

// envVarNames are the memkey secret keys delivered per companion as
// PW_<UPPER_SNAKE(name)>_<KEY>. Consumption contract: the gateway maps
// PW_<NAME>_OPENVIKING_API_KEY to the child process's OPENVIKING_API_KEY, and
// the companion process dials OpenViking directly at OPENVIKING_BASE_URL.
const (
	memkeyKey     = "OPENVIKING_API_KEY"
	memkeyToken   = "WORKSPACE_INTERNAL_TOKEN"
	apiKeyEnvName = "BENCH_API_KEY"
	apiKeyDataKey = "BIGMODEL_API_KEY"
)

func envName(companion, key string) string {
	// PW_<UPPER_SNAKE>_<KEY>: bash-sourceable, collision-free per companion.
	up := strings.ToUpper(strings.ReplaceAll(companion, "-", "_"))
	return "PW_" + up + "_" + key
}

// BuildPayload assembles the config-disk content. Secrets land only in the
// env file (written 0600 by the disk layer; never logged).
func BuildPayload(in Inputs) (*ConfigPayload, error) {
	ws := in.Workspace
	if in.Manifest == "" {
		return nil, fmt.Errorf("companions manifest is empty (ConfigMap missing?)")
	}

	// Derive the guest /30 members from the operator-assigned subnet:
	// <base>.1 = tap host, <base>.2 = guest.
	hostIP, guestIP, err := subnetEndpoints(ws.Status.VMSubnet)
	if err != nil {
		return nil, err
	}

	primary := ""
	if len(ws.Spec.Companions) > 0 {
		primary = ws.Spec.Companions[0].Model
	}
	if primary == "" {
		primary = "glm-4.6"
	}

	lines := []string{
		envPort,
		envCompanionsKey + "=" + envCompanionsVal,
		envDataDir,
		envChildPortBase,
		envIdleMS,
		"BENCH_BASE_URL=" + in.ModelBaseURL,
		"BENCH_MODEL=" + primary,
		"PW_WORKSPACE=" + ws.Name,
		"WORKSPACE_DESKTOP_ENABLED=" + map[bool]string{true: "1", false: "0"}[ws.Spec.Desktop.Enabled],
		"PW_GUEST_IP=" + guestIP,
		"PW_GUEST_GW=" + hostIP,
		"PW_GUEST_PREFIX=30",
	}
	if in.LifecycleToken != "" {
		lines = append(lines, "WORKSPACE_LIFECYCLE_TOKEN="+in.LifecycleToken)
	}
	if in.MemoryBaseURL != "" {
		lines = append(lines, "OPENVIKING_BASE_URL="+in.MemoryBaseURL)
	}
	if in.Credentials != nil {
		if key := in.Credentials.Data[apiKeyDataKey]; len(key) > 0 {
			lines = append(lines, apiKeyEnvName+"="+string(key))
		}
	}
	// Per-companion memkey delivery, roster order for stable hashes.
	names := make([]string, 0, len(ws.Spec.Companions))
	for i := range ws.Spec.Companions {
		names = append(names, ws.Spec.Companions[i].Name)
	}
	sort.Strings(names)
	for _, name := range names {
		secret := in.Memkeys[name]
		if secret == nil {
			continue
		}
		if v := secret.Data[memkeyKey]; len(v) > 0 {
			lines = append(lines, envName(name, memkeyKey)+"="+string(v))
		}
		if v := secret.Data[memkeyToken]; len(v) > 0 {
			lines = append(lines, envName(name, memkeyToken)+"="+string(v))
		}
	}

	resolv := "nameserver " + in.DNSIP + "\n"
	if in.DNSSearch != "" {
		resolv += "search " + in.DNSSearch + "\n"
	}

	manifest := []byte(in.Manifest)
	envFile := []byte(strings.Join(lines, "\n") + "\n")
	resolvConf := []byte(resolv)

	sum := sha256.New()
	sum.Write(manifest)
	sum.Write([]byte{0})
	sum.Write(envFile)
	sum.Write([]byte{0})
	sum.Write(resolvConf)

	return &ConfigPayload{
		CompanionsJSON: manifest,
		EnvFile:        envFile,
		ResolvConf:     resolvConf,
		Hash:           hex.EncodeToString(sum.Sum(nil))[:16],
	}, nil
}

// subnetEndpoints returns the .1 (host/tap) and .2 (guest) addresses of a
// /30 in "a.b.c.d/30" form.
func subnetEndpoints(subnet string) (host, guest string, err error) {
	i := strings.IndexByte(subnet, '/')
	if i < 0 || !strings.HasSuffix(subnet, "/30") {
		return "", "", fmt.Errorf("vmSubnet %q is not a /30", subnet)
	}
	base := subnet[:i]
	parts := strings.Split(base, ".")
	if len(parts) != 4 {
		return "", "", fmt.Errorf("vmSubnet %q is not IPv4", subnet)
	}
	last := parts[3]
	host = parts[0] + "." + parts[1] + "." + parts[2] + "." + lastPlus(last, 1)
	guest = parts[0] + "." + parts[1] + "." + parts[2] + "." + lastPlus(last, 2)
	if host == "" || guest == "" {
		return "", "", fmt.Errorf("vmSubnet %q overflow", subnet)
	}
	return host, guest, nil
}

func lastPlus(octet string, n int) string {
	var v int
	if _, err := fmt.Sscanf(octet, "%d", &v); err != nil {
		return ""
	}
	v += n
	if v < 0 || v > 255 {
		return ""
	}
	return fmt.Sprintf("%d", v)
}

// GuestIP is the guest address of the workspace's /30 (exposed for the
// healthz probe and the TCP proxy backend dial).
func GuestIP(subnet string) (string, error) {
	_, guest, err := subnetEndpoints(subnet)
	return guest, err
}

// HostIP is the tap-side address of the workspace's /30.
func HostIP(subnet string) (string, error) {
	host, _, err := subnetEndpoints(subnet)
	return host, err
}

// GuestMAC derives a stable locally-administered unicast MAC from the /30
// (06:00:AC:<folded subnet bytes>) — deterministic per subnet, no state.
func GuestMAC(subnet string) string {
	base := strings.TrimSuffix(subnet, "/30")
	octets := strings.Split(base, ".")
	parts := []string{"06", "00", "AC"}
	var fold byte
	for _, o := range octets {
		v, _ := strconv.Atoi(o)
		fold ^= byte(v)
		parts = append(parts, fmt.Sprintf("%02X", v))
	}
	// 3 fixed + 4 octet bytes = 7; fold into 6 groups (locally administered).
	parts[5] = fmt.Sprintf("%02X", mustParseByte(parts[5])^fold)
	return strings.Join(parts[:6], ":")
}

func mustParseByte(hex2 string) byte {
	v, _ := strconv.ParseUint(hex2, 16, 8)
	return byte(v)
}
