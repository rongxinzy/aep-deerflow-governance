// Package manager is the workspace-agent core: one Supervisor per assigned
// microvm workspace plus the resync loop that reconciles desired state
// (CR + manifest + secrets + endpoints) into VMs, taps, whitelist chains,
// disks and the TCP proxy.
//
// Status single-writer convention (operator owns nodeName/vmSubnet; this
// agent owns phase/gatewayEndpoint/vm on the microvm path) is enforced by
// patching exactly those fields.
package manager

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/alloc"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/disk"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/hostcmd"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/iptables"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/proxy"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/render"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/tap"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/template"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/vm"
)

// AgentFinalizer guards node-side teardown (VM, tap, chains, disks, dir).
const AgentFinalizer = "digitalemployee.zhiyuan.io/workspace-node-agent"

// Boot-failure policy. Vars (not consts) so tests can shrink the windows.
var (
	// maxBootFails is the give-up threshold for consecutive boot failures.
	maxBootFails = 5
	// giveUpRetryInterval is how long a given-up supervisor waits before one
	// retry attempt (self-heal: no human spec change required).
	giveUpRetryInterval = time.Hour
	// stableResetDuration is how long a VM must stay continuously Running
	// before bootFails is forgiven.
	stableResetDuration = time.Hour
)

// Config wires the manager.
type Config struct {
	Namespace      string
	NodeName       string
	HostIP         string
	Workdir        string // hostPath root: templates/ disks/ run/
	TemplateTag    string
	PortLo, PortHi int32
	ModelBaseURL   string
	MemoryBaseURL  string // OpenViking service; rendered as OPENVIKING_BASE_URL on the config disk
	SerialLogMB    int
}

// Manager owns the supervisor set.
type Manager struct {
	cfg    Config
	client client.Client
	runner hostcmd.Runner
	disks  disk.DiskIO
	// NewDriver builds a driver per run dir (tests swap fakes in).
	NewDriver func(runDir string) vm.Driver

	mu     sync.Mutex
	supers map[string]*Supervisor
	// phases is the /metrics source for pw_node_agent_workspace_phase, keyed by
	// workspace name and refreshed from every CR List — including workspaces
	// whose reconcile fails before a supervisor exists (early provisioning
	// failures would otherwise emit no series at all and WorkspacePhaseStuck
	// could never fire). Entries are pruned when the CR leaves the assigned
	// set (Resync orphan sweep) and on teardown.
	phases    map[string]string
	usedPorts func() []int32 // extra occupied ports (proxies of live supervisors)

	resyncErrors atomic.Int64 // rounds that returned an error (metrics)
}

// New builds the manager.
func New(cfg Config, c client.Client, runner hostcmd.Runner, disks disk.DiskIO, newDriver func(string) vm.Driver) *Manager {
	m := &Manager{cfg: cfg, client: c, runner: runner, disks: disks, NewDriver: newDriver,
		supers: map[string]*Supervisor{}, phases: map[string]string{}}
	m.usedPorts = func() []int32 { return m.livePorts() }
	return m
}

func (m *Manager) livePorts() []int32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []int32
	for _, s := range m.supers {
		if s.hostPort != 0 {
			out = append(out, s.hostPort)
		}
	}
	return out
}

// Supervisor is one workspace's runtime handle.
//
// Concurrency: /metrics scrapes (collector) run concurrently with the resync
// loop. The scalar fields below (hostPort, configHash, bootFails, lastBoot,
// runningSince) are read/written only under Manager.mu — short critical
// sections around the value copy; long operations (driver.Start/Stop, HTTP
// healthz, disk writes, API patches) stay outside the lock. driver and proxy
// carry their own synchronization: vm.Driver implementations are required to
// be safe for concurrent use (the scrape's Alive races reconcile's Stop), and
// proxy swaps follow stop-outside-lock + publish-under-lock.
type Supervisor struct {
	name       string
	subnet     string // assigned /30 (full teardown incl. NAT rule)
	driver     vm.Driver
	proxy      *proxy.TCPProxy
	hostPort   int32
	configHash string // hash the running VM was booted with
	bootFails  int
	lastBoot   time.Time
	// runningSince marks the start of the current uninterrupted healthy
	// stretch; zeroed whenever the guest fails healthz. After
	// stableResetDuration of stability, bootFails is forgiven.
	runningSince time.Time
}

// Resync lists workspaces, reconciles assigned ones, tears down orphans.
func (m *Manager) Resync(ctx context.Context) (firstErr error) {
	// Every finished round heartbeats, error or not (backstop for the
	// per-workspace heartbeats inside the loop below): the heartbeat feeds
	// the DaemonSet liveness probe, whose blast radius is every VM on the
	// node. A workspace stuck in give-up makes Resync return an error
	// indefinitely; gating the heartbeat on success would misread a healthy
	// loop as wedged.
	defer func() {
		m.writeHeartbeat()
		if firstErr != nil {
			m.resyncErrors.Add(1)
		}
	}()
	var list v1alpha1.PersonalWorkspaceList
	if err := m.client.List(ctx, &list, client.InNamespace(m.cfg.Namespace)); err != nil {
		return err
	}
	assigned := map[string]bool{}
	for i := range list.Items {
		ws := &list.Items[i]
		if ws.Spec.Runtime != "microvm" || ws.Status.NodeName != m.cfg.NodeName {
			continue
		}
		assigned[ws.Name] = true
		if err := m.reconcileAssigned(ctx, ws); err != nil {
			firstErr = fmt.Errorf("%s: %w", ws.Name, err)
		}
	}
	// Orphan teardown: supervisor exists but CR no longer ours.
	m.mu.Lock()
	var orphans []string
	for name := range m.supers {
		if !assigned[name] {
			orphans = append(orphans, name)
		}
	}
	// Phase entries track assigned CRs; a CR force-deleted past its finalizer
	// would otherwise leak its series forever.
	for name := range m.phases {
		if !assigned[name] {
			delete(m.phases, name)
		}
	}
	m.mu.Unlock()
	for _, name := range orphans {
		if err := m.teardownWorkspace(ctx, name); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s (orphan): %w", name, err)
		}
	}
	return firstErr
}

// setPhase records the CR phase of an assigned workspace for /metrics. The
// map (not the supervisor set) is the gauge's source, so failures before
// supervisor creation still emit a series.
func (m *Manager) setPhase(name, phase string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.phases[name] = phase
}

// reconcileAssigned reconciles one assigned workspace, records its phase
// (even when reconcile fails before a supervisor exists — failStatus writes
// Failed to the ws copy we hold, and the /metrics phase gauge reads the map,
// not the supervisor set), and heartbeats right after.
//
// Heartbeat per workspace, not only per round: one round's duration grows
// linearly with broken VMs (healthz 2s + stop 15s + boot 30s each), and ~20
// wedged guests would exceed the liveness probe's staleness window and power
// off every VM on the node. Worst-case interval is now a single workspace's
// processing time; the Resync defer remains as the empty-list backstop.
func (m *Manager) reconcileAssigned(ctx context.Context, ws *v1alpha1.PersonalWorkspace) error {
	err := m.reconcile(ctx, ws)
	m.setPhase(ws.Name, string(ws.Status.Phase))
	if err != nil {
		// Startup failures before VM supervision must be visible to the
		// owner too. Keep detailed host errors in logs, not the user UI.
		if ws.DeletionTimestamp.IsZero() && ws.Status.Phase != v1alpha1.WorkspacePhaseRunning && ws.Status.Phase != v1alpha1.WorkspacePhaseFailed {
			if perr := m.patchStatus(ctx, ws, func(s *v1alpha1.PersonalWorkspaceStatus) {
				s.Phase = v1alpha1.WorkspacePhaseFailed
				s.Message = "云电脑准备失败，系统将自动重试。请稍后重新检查，或联系管理员。"
			}); perr == nil {
				m.setPhase(ws.Name, string(v1alpha1.WorkspacePhaseFailed))
			}
		}
	}
	m.writeHeartbeat()
	return err
}

// writeHeartbeat stamps the resync loop's liveness file under the run dir.
// Best-effort: a persistently unwritable workdir stales the heartbeat and
// surfaces through the liveness probe instead of a log line nobody reads.
func (m *Manager) writeHeartbeat() {
	dir := filepath.Join(m.cfg.Workdir, "run")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "agent.heartbeat"),
		[]byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o644)
}

// manifestEntry mirrors companionManifestEntry (wire contract of
// companions.json; MemkeySecret names the secret to deliver).
type manifestEntry struct {
	Name         string `json:"name"`
	MemkeySecret string `json:"memkeySecret"`
}

type manifest struct {
	Companions []manifestEntry `json:"companions"`
}

func (m *Manager) reconcile(ctx context.Context, ws *v1alpha1.PersonalWorkspace) error {
	if !ws.DeletionTimestamp.IsZero() {
		return m.finalize(ctx, ws)
	}
	if err := m.ensureFinalizer(ctx, ws); err != nil {
		return err
	}
	// Inputs: manifest CM, memkey secrets, credentials, dns service.
	cm := &corev1.ConfigMap{}
	if err := m.client.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: "pw-" + ws.Name + "-companions"}, cm); err != nil {
		return fmt.Errorf("manifest configmap: %w", err)
	}
	manifestJSON := cm.Data["companions.json"]
	var mf manifest
	if err := json.Unmarshal([]byte(manifestJSON), &mf); err != nil {
		return fmt.Errorf("manifest parse: %w", err)
	}
	memkeys := map[string]*corev1.Secret{}
	for _, entry := range mf.Companions {
		if entry.MemkeySecret == "" {
			continue
		}
		sec := &corev1.Secret{}
		if err := m.client.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: entry.MemkeySecret}, sec); err != nil {
			return fmt.Errorf("memkey secret %s: %w", entry.MemkeySecret, err)
		}
		memkeys[entry.Name] = sec
	}
	creds := &corev1.Secret{}
	if ws.Spec.Credentials.Name != "" {
		if err := m.client.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: ws.Spec.Credentials.Name}, creds); err != nil {
			return fmt.Errorf("credentials secret: %w", err)
		}
	}
	dnsIP, err := m.dnsIP(ctx)
	if err != nil {
		return err
	}
	runDir := filepath.Join(m.cfg.Workdir, "run", ws.Name)
	lifecycleToken, err := guestLifecycleToken(runDir)
	if err != nil {
		return err
	}
	payload, err := render.BuildPayload(render.Inputs{
		LifecycleToken: lifecycleToken,
		Workspace:      ws, Manifest: manifestJSON, Memkeys: memkeys, Credentials: creds,
		ModelBaseURL: m.cfg.ModelBaseURL, MemoryBaseURL: m.cfg.MemoryBaseURL, DNSIP: dnsIP,
		DNSSearch: m.cfg.Namespace + ".svc.cluster.local svc.cluster.local",
	})
	if err != nil {
		return err
	}

	// Egress targets from endpoints.
	targets, err := m.egressTargets(ctx, ws)
	if err != nil {
		return err
	}

	diskDir := filepath.Join(m.cfg.Workdir, "disks", ws.Name)
	tag := ws.Spec.TemplateTag
	if tag == "" {
		tag = m.cfg.TemplateTag
	}
	assets, err := template.Resolve(filepath.Join(m.cfg.Workdir, "templates"), tag)
	if err == nil && ws.Spec.Desktop.Enabled && !assets.DesktopAvailable {
		err = fmt.Errorf("template %s does not declare desktop support", tag)
	}
	if err != nil {
		_ = m.patchStatus(ctx, ws, func(s *v1alpha1.PersonalWorkspaceStatus) {
			s.Message = "configuration not applied: " + err.Error()
		})
		return err
	}
	// Include machine shape and template in the rollout identity, not only env.
	shape := fmt.Sprintf("%s-%s-%d-%s-%s", payload.Hash, tag, ws.Spec.Resources.VCPUs, ws.Spec.Resources.Memory, ws.Spec.Resources.Disk)
	payload.Hash = fmt.Sprintf("%x", sha256.Sum256([]byte(shape)))[:16]
	if ws.Spec.Desktop.Enabled {
		memory, err := parseQuantity(ws.Spec.Resources.Memory)
		if err != nil || memory < 4096 || ws.Spec.Resources.VCPUs < 2 {
			return fmt.Errorf("desktop requires at least 2 vcpus and 4Gi memory")
		}
	}
	dev, err := tap.New(ws.Name, ws.Status.VMSubnet)
	if err != nil {
		return err
	}
	if err := dev.Ensure(ctx, m.runner); err != nil {
		return err
	}
	if err := iptables.EnsureEgress(ctx, m.runner, ws.Name, targets); err != nil {
		return err
	}

	// Port: keep recorded, else allocate. Occupied = live proxies + every
	// hostPort recorded in sibling CR status (cross-restart stickiness).
	port := int32(0)
	if ws.Status.VM != nil {
		port = ws.Status.VM.HostPort
	}
	if port == 0 {
		var siblings v1alpha1.PersonalWorkspaceList
		if err := m.client.List(ctx, &siblings, client.InNamespace(m.cfg.Namespace)); err != nil {
			return err
		}
		used := m.usedPorts()
		for i := range siblings.Items {
			if v := siblings.Items[i].Status.VM; v != nil && v.HostPort != 0 {
				used = append(used, v.HostPort)
			}
		}
		port, err = alloc.AllocateFree(used, m.cfg.PortLo, m.cfg.PortHi, alloc.ProbePort)
		if err != nil {
			return err
		}
	}
	guestIP, err := render.GuestIP(ws.Status.VMSubnet)
	if err != nil {
		return err
	}

	m.mu.Lock()
	sup := m.supers[ws.Name]
	if sup == nil {
		sup = &Supervisor{name: ws.Name, subnet: ws.Status.VMSubnet, driver: m.NewDriver(runDir)}
		if ws.Status.VM != nil {
			sup.configHash = ws.Status.VM.ConfigHash
		}
		m.supers[ws.Name] = sup
	}
	m.mu.Unlock()

	state := m.supSnapshot(sup)
	needsBoot := !sup.driver.Alive()
	// Config rollout: VM must be stopped while the config disk is written
	// (hot-write corruption guard) — reboot when the desired hash differs.
	if sup.driver.Alive() && state.configHash != "" && state.configHash != payload.Hash {
		prepareGuestStop(guestIP, lifecycleToken)
		if err := sup.driver.Stop(15 * time.Second); err != nil {
			return err
		}
		needsBoot = true
	}
	if needsBoot {
		if err := m.boot(ctx, ws, sup, payload, assets, dev, diskDir, runDir, guestIP, port); err != nil {
			return m.failStatus(ctx, ws, err)
		}
	} else {
		state = m.supSnapshot(sup)
		if state.hostPort != port || !state.hasProxy {
			// Driver alive from a previous process life (adoption path rebuilds
			// the proxy): rebind.
			if p := m.takeProxy(sup); p != nil {
				p.Stop()
			}
			if err := m.startProxy(sup, port, guestIP); err != nil {
				return m.failStatus(ctx, ws, err)
			}
		}
	}

	// Steady state: healthz. A dead process reboots via needsBoot above; a
	// hung gateway (process alive, healthz failing) gets the same reboot
	// semantics: stop now, count the failure, next resync backs off.
	if !probeHealthz(guestIP, 2*time.Second) {
		m.noteHealthzFailure(sup)
		if err := sup.driver.Stop(15 * time.Second); err != nil {
			return err
		}
		return m.patchStatus(ctx, ws, func(s *v1alpha1.PersonalWorkspaceStatus) {
			if s.Phase != v1alpha1.WorkspacePhaseFailed {
				s.Phase = v1alpha1.WorkspacePhasePending
				s.Message = "guest gateway healthz failing; reboot scheduled with backoff"
			}
		})
	}
	// Healthy stretch: forgive bootFails once the guest has been stable past
	// the reset window (transient flaps must not march the counter to give-up).
	m.noteHealthy(sup)
	return m.patchStatus(ctx, ws, func(s *v1alpha1.PersonalWorkspaceStatus) {
		s.Phase = v1alpha1.WorkspacePhaseRunning
		s.Message = ""
		s.GatewayEndpoint = m.cfg.HostIP + ":" + strconv.Itoa(int(port))
		s.VM = &v1alpha1.WorkspaceVMStatus{HostPort: port, ConfigHash: payload.Hash, TemplateTag: tag, Resources: ws.Spec.Resources, DesktopEnabled: ws.Spec.Desktop.Enabled}
	})
}

// supState is a locked snapshot of the supervisor scalars reconcile reads
// (see the Supervisor concurrency doc); runningSince is only mutated via
// noteHealthy/noteHealthzFailure and never read directly.
type supState struct {
	hostPort   int32
	configHash string
	hasProxy   bool
	bootFails  int
	lastBoot   time.Time
}

func (m *Manager) supSnapshot(sup *Supervisor) supState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return supState{
		hostPort: sup.hostPort, configHash: sup.configHash, hasProxy: sup.proxy != nil,
		bootFails: sup.bootFails, lastBoot: sup.lastBoot,
	}
}

// noteBootFailure counts a failed boot attempt.
func (m *Manager) noteBootFailure(sup *Supervisor) {
	m.mu.Lock()
	sup.bootFails++
	sup.lastBoot = time.Now()
	m.mu.Unlock()
}

// noteBootSuccess resets the failure bookkeeping after a healthy boot.
func (m *Manager) noteBootSuccess(sup *Supervisor, configHash string) {
	m.mu.Lock()
	sup.bootFails = 0
	sup.lastBoot = time.Now()
	sup.runningSince = time.Now()
	sup.configHash = configHash
	m.mu.Unlock()
}

// noteHealthzFailure counts a steady-state healthz failure and ends the
// healthy stretch.
func (m *Manager) noteHealthzFailure(sup *Supervisor) {
	m.mu.Lock()
	sup.bootFails++
	sup.lastBoot = time.Now()
	sup.runningSince = time.Time{}
	m.mu.Unlock()
}

// noteHealthy advances the healthy stretch and forgives bootFails once it has
// lasted past the reset window.
func (m *Manager) noteHealthy(sup *Supervisor) {
	m.mu.Lock()
	if sup.runningSince.IsZero() {
		sup.runningSince = time.Now()
	}
	if sup.bootFails > 0 && time.Since(sup.runningSince) >= stableResetDuration {
		sup.bootFails = 0
	}
	m.mu.Unlock()
}

// takeProxy detaches the current proxy under the lock so the caller can stop
// it outside the critical section.
func (m *Manager) takeProxy(sup *Supervisor) *proxy.TCPProxy {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := sup.proxy
	sup.proxy = nil
	return p
}

func (m *Manager) boot(ctx context.Context, ws *v1alpha1.PersonalWorkspace, sup *Supervisor,
	payload *render.ConfigPayload, assets *template.Assets, dev *tap.Device,
	diskDir, runDir, guestIP string, port int32) error {
	// Backoff after repeated boot failures. Past the give-up threshold the
	// supervisor still self-heals: one retry per giveUpRetryInterval (a spec
	// change is no longer the only way out).
	state := m.supSnapshot(sup)
	if state.bootFails >= maxBootFails && time.Since(state.lastBoot) < giveUpRetryInterval {
		return fmt.Errorf("boot failed %d consecutive times; giving up (hourly self-heal retry armed, serial log: %s)", state.bootFails, filepath.Join(runDir, "serial.log"))
	}
	if delay := backoff(state.bootFails); delay > 0 && time.Since(state.lastBoot) < delay {
		return fmt.Errorf("boot backoff (%s)", delay)
	}
	if p := m.takeProxy(sup); p != nil {
		p.Stop()
	}
	if err := m.disks.EnsureDataDisk(ctx, filepath.Join(diskDir, "data.ext4"), diskSizeBytes(ws)); err != nil {
		return err
	}
	// Config disk write requires the VM stopped (state machine guarantees:
	// boot() is only called with !Alive()).
	if sup.driver.Alive() {
		if err := sup.driver.Stop(15 * time.Second); err != nil {
			return err
		}
	}
	if err := m.disks.WriteConfigDisk(ctx, filepath.Join(diskDir, "config.ext4"), payload); err != nil {
		return err
	}
	memMib := int32(1024)
	if q, err := parseQuantity(ws.Spec.Resources.Memory); err == nil && q > 0 {
		memMib = int32(q)
	}
	vcpus := ws.Spec.Resources.VCPUs
	if vcpus <= 0 {
		vcpus = 2
	}
	if err := sup.driver.Start(vm.Config{
		Workspace: ws.Name, Binary: assets.FcPath, Kernel: assets.KernelPath,
		Rootfs: assets.RootfsPath, ConfigDisk: filepath.Join(diskDir, "config.ext4"),
		DataDisk: filepath.Join(diskDir, "data.ext4"), TapDevice: dev.Name,
		GuestMAC: render.GuestMAC(ws.Status.VMSubnet), VCPUs: vcpus, MemMib: memMib,
		RunDir: runDir, SerialLogMB: m.cfg.SerialLogMB, BootTimeout: 30 * time.Second,
	}); err != nil {
		m.noteBootFailure(sup)
		return err
	}
	// Wait for the in-VM gateway.
	if !waitForHealthz(guestIP, 30*time.Second) {
		m.noteBootFailure(sup)
		return fmt.Errorf("guest gateway did not become healthy in 30s")
	}
	m.noteBootSuccess(sup, payload.Hash)
	if err := m.startProxy(sup, port, guestIP); err != nil {
		return err
	}
	return nil
}

func (m *Manager) startProxy(sup *Supervisor, port int32, guestIP string) error {
	p := proxy.New(fmt.Sprintf(":%d", port), guestIP+":8080")
	if err := p.Start(); err != nil {
		return fmt.Errorf("proxy bind :%d: %w", port, err)
	}
	m.mu.Lock()
	sup.proxy = p
	sup.hostPort = port
	m.mu.Unlock()
	return nil
}

// finalize tears down node state and removes the agent finalizer.
func (m *Manager) finalize(ctx context.Context, ws *v1alpha1.PersonalWorkspace) error {
	if err := m.teardownWorkspace(ctx, ws.Name); err != nil {
		return err
	}
	if !containsFinalizer(ws.Finalizers, AgentFinalizer) {
		return nil
	}
	old := ws.DeepCopy()
	ws.Finalizers = removeFinalizer(ws.Finalizers, AgentFinalizer)
	return m.client.Patch(ctx, ws, client.MergeFrom(old))
}

func (m *Manager) teardownWorkspace(ctx context.Context, name string) error {
	m.mu.Lock()
	sup := m.supers[name]
	if sup != nil {
		delete(m.supers, name)
	}
	delete(m.phases, name) // the phase map tracks assigned CRs; teardown drops both
	var p *proxy.TCPProxy
	if sup != nil {
		p = sup.proxy
		sup.proxy = nil
	}
	m.mu.Unlock()
	// Orphaned node state (crash before supervisor creation, or agent
	// restart with the CR since reassigned): teardown by name.
	if sup != nil && sup.driver.Alive() {
		if err := sup.driver.Stop(15 * time.Second); err != nil {
			return err
		}
	}
	if p != nil {
		p.Stop()
	}
	if err := iptables.TeardownEgress(ctx, m.runner, name); err != nil {
		return err
	}
	if sup != nil {
		if dev, err := tap.New(name, sup.subnet); err == nil {
			_ = dev.Teardown(ctx, m.runner)
		}
	} else {
		_ = tap.TeardownByName(ctx, m.runner, name)
	}
	// Disks and run dir go too — data disk is workspace-scoped, deletion is
	// the CR deletion contract (memory users are cleaned by the operator).
	for _, dir := range []string{filepath.Join(m.cfg.Workdir, "disks", name), filepath.Join(m.cfg.Workdir, "run", name)} {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) ensureFinalizer(ctx context.Context, ws *v1alpha1.PersonalWorkspace) error {
	if containsFinalizer(ws.Finalizers, AgentFinalizer) {
		return nil
	}
	old := ws.DeepCopy()
	ws.Finalizers = append(ws.Finalizers, AgentFinalizer)
	return m.client.Patch(ctx, ws, client.MergeFrom(old))
}

func (m *Manager) patchStatus(ctx context.Context, ws *v1alpha1.PersonalWorkspace, mutate func(*v1alpha1.PersonalWorkspaceStatus)) error {
	old := ws.DeepCopy()
	mutate(&ws.Status)
	return m.client.Status().Patch(ctx, ws, client.MergeFrom(old))
}

func (m *Manager) failStatus(ctx context.Context, ws *v1alpha1.PersonalWorkspace, cause error) error {
	err := m.patchStatus(ctx, ws, func(s *v1alpha1.PersonalWorkspaceStatus) {
		s.Phase = v1alpha1.WorkspacePhaseFailed
		s.Message = cause.Error()
	})
	if err != nil {
		return err
	}
	return cause
}

func (m *Manager) dnsIP(ctx context.Context) (string, error) {
	svc := &corev1.Service{}
	if err := m.client.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: "kube-dns"}, svc); err != nil {
		return "", fmt.Errorf("kube-dns service: %w", err)
	}
	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == "None" {
		return "", fmt.Errorf("kube-dns has no cluster IP")
	}
	return svc.Spec.ClusterIP, nil
}

// egressTargets resolves the workspace's egress classes to endpoint pod IPs.
func (m *Manager) egressTargets(ctx context.Context, ws *v1alpha1.PersonalWorkspace) ([]iptables.Target, error) {
	allow := ws.Spec.Security.EgressAllow
	var targets []iptables.Target
	for _, class := range iptables.ClassesFor(allow) {
		key, ok := iptables.ServiceFor(class)
		if !ok {
			continue
		}
		eps := &corev1.Endpoints{}
		if err := m.client.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: key.Name}, eps); err != nil {
			return nil, fmt.Errorf("endpoints %s/%s: %w", key.Namespace, key.Name, err)
		}
		for _, sub := range eps.Subsets {
			for _, addr := range sub.Addresses {
				targets = append(targets, iptables.Target{IP: addr.IP, Port: key.Port, Proto: key.Proto})
			}
		}
	}
	return targets, nil
}

func backoff(fails int) time.Duration {
	switch {
	case fails <= 0:
		return 0
	case fails == 1:
		return 5 * time.Second
	case fails == 2:
		return 10 * time.Second
	case fails == 3:
		return 20 * time.Second
	case fails == 4:
		return 40 * time.Second
	default:
		return 60 * time.Second
	}
}

func probeHealthz(guestIP string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get("http://" + guestIP + ":8080/healthz")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func waitForHealthz(guestIP string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if probeHealthz(guestIP, 2*time.Second) {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// diskSizeBytes resolves spec.resources.disk to bytes (default 5 GiB).
func diskSizeBytes(ws *v1alpha1.PersonalWorkspace) int64 {
	mib, err := parseQuantity(ws.Spec.Resources.Disk)
	if err != nil || mib <= 0 {
		return 5 << 30
	}
	return mib << 20
}

// parseQuantity parses simple quantity strings ("5Gi", "512Mi") to MiB.
func parseQuantity(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty quantity")
	}
	unit := s[len(s)-2:]
	num, err := strconv.ParseInt(strings.TrimSuffix(s, unit), 10, 64)
	if err != nil {
		return 0, err
	}
	switch strings.ToUpper(unit) {
	case "GI":
		return num * 1024, nil
	case "MI":
		return num, nil
	default:
		return 0, fmt.Errorf("unsupported quantity unit %q", unit)
	}
}

func containsFinalizer(list []string, f string) bool {
	for _, v := range list {
		if v == f {
			return true
		}
	}
	return false
}

func removeFinalizer(list []string, f string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v != f {
			out = append(out, v)
		}
	}
	return out
}
