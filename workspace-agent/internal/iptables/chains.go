// Package iptables renders the per-workspace egress whitelist chain
// (E3b pattern): ESTABLISHED first, DNS endpoints (implicit), the
// egressAllow classes resolved to service endpoint pod IPs, REJECT last;
// hooked from FORWARD -i tap-<ws>. The agent only ever touches its own
// chains — never flushes FORWARD.
package iptables

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/hostcmd"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/tap"
)

// Target is one allowed destination (a resolved service endpoint).
type Target struct {
	IP   string
	Port string // "53", "80", "1933"...
	// Proto "udp" or "tcp" ("both" expands to two rules).
	Proto string
}

// EgressClass names a platform dependency the workspace may reach. The
// classes mirror spec.security.egressAllow; "model-gateway" is implicit
// (a workspace without model access is useless in P1).
const (
	ClassModelGateway = "model-gateway"
	ClassMemory       = "memory"
)

// ServiceKey identifies the backing service for a class.
type ServiceKey struct {
	Namespace string
	Name      string
	Port      string // service port
	Proto     string // endpoint proto: "tcp" (services) or "both" (dns)
}

// ClassServices maps egress classes to their backing services; the agent
// resolves endpoints for these.
var ClassServices = map[string]ServiceKey{
	ClassModelGateway: {Namespace: "higress-system", Name: "higress-gateway", Port: "80", Proto: "tcp"},
	ClassMemory:       {Namespace: "memory-system", Name: "openviking", Port: "1933", Proto: "tcp"},
}

// DNSClass is the implicit kube-dns dependency of every workspace.
const DNSClass = "dns"

var dnsService = ServiceKey{Namespace: "kube-system", Name: "kube-dns", Port: "53", Proto: "both"}

// ChainID is the iptables chain name for a workspace (≤29 chars: FC_WL_ + name).
func ChainID(workspace string) string {
	name := workspace
	if len(name) > 22 {
		name = name[:22]
	}
	return "FC_WL_" + name
}

// Rules renders the ordered rule set of a chain. Deterministic:
// ESTABLISHED → sorted targets → REJECT.
func Rules(chain string, targets []Target) []string {
	rules := []string{
		"-A " + chain + " -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
	}
	sorted := append([]Target(nil), targets...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].IP != sorted[j].IP {
			return sorted[i].IP < sorted[j].IP
		}
		if sorted[i].Port != sorted[j].Port {
			return sorted[i].Port < sorted[j].Port
		}
		return sorted[i].Proto < sorted[j].Proto
	})
	seen := map[string]bool{}
	for _, t := range sorted {
		protos := []string{t.Proto}
		if t.Proto == "both" {
			protos = []string{"udp", "tcp"}
		}
		for _, proto := range protos {
			rule := fmt.Sprintf("-A %s -p %s -d %s --dport %s -j ACCEPT", chain, proto, t.IP, t.Port)
			if !seen[rule] {
				seen[rule] = true
				rules = append(rules, rule)
			}
		}
	}
	rules = append(rules, "-A "+chain+" -j REJECT")
	return rules
}

func applyRules(ctx context.Context, run hostcmd.Runner, chain string, targets []Target) error {
	for _, rule := range Rules(chain, targets) {
		if _, err := run.Run(ctx, "iptables", strings.Fields(rule)...); err != nil {
			return fmt.Errorf("rule %q: %w", rule, err)
		}
	}
	return nil
}

// EnsureEgress installs/refreshes the chain: the desired rules are built in
// a scratch chain first and only flushed into the live chain once complete,
// so a partial render never widens the whitelist (fail-open to the previous
// chain on error). The FORWARD jump is added idempotently.
func EnsureEgress(ctx context.Context, run hostcmd.Runner, workspace string, targets []Target) error {
	chain := ChainID(workspace)
	scratch := chain + "_N"
	// Scratch hygiene, then build the desired state there.
	_, _ = run.Run(ctx, "iptables", "-F", scratch)
	_, _ = run.Run(ctx, "iptables", "-X", scratch)
	if _, err := run.Run(ctx, "iptables", "-N", scratch); err != nil {
		return fmt.Errorf("scratch chain %s: %w", scratch, err)
	}
	if err := applyRules(ctx, run, scratch, targets); err != nil {
		_, _ = run.Run(ctx, "iptables", "-F", scratch)
		_, _ = run.Run(ctx, "iptables", "-X", scratch)
		return err
	}
	// Swap: flush live, re-add from the (already validated) rule set. A
	// failure mid-swap is repaired by the next resync.
	if _, err := run.Run(ctx, "iptables", "-L", chain); err != nil {
		if _, err := run.Run(ctx, "iptables", "-N", chain); err != nil {
			return fmt.Errorf("chain create %s: %w", chain, err)
		}
	}
	if _, err := run.Run(ctx, "iptables", "-F", chain); err != nil {
		return fmt.Errorf("chain flush %s: %w", chain, err)
	}
	if err := applyRules(ctx, run, chain, targets); err != nil {
		return err
	}
	_, _ = run.Run(ctx, "iptables", "-F", scratch)
	_, _ = run.Run(ctx, "iptables", "-X", scratch)
	// FORWARD jump (idempotent via -C).
	jump := []string{"-C", "FORWARD", "-i", tap.InterfaceName(workspace), "-j", chain}
	if _, err := run.Run(ctx, "iptables", jump...); err != nil {
		if _, err := run.Run(ctx, "iptables", "-I", "FORWARD", "1", "-i", tap.InterfaceName(workspace), "-j", chain); err != nil {
			return fmt.Errorf("forward jump for %s: %w", chain, err)
		}
	}
	return nil
}

// TeardownEgress removes the FORWARD jump, flushes and deletes the chain.
// Idempotent; missing pieces are the desired end state.
func TeardownEgress(ctx context.Context, run hostcmd.Runner, workspace string) error {
	chain := ChainID(workspace)
	_, _ = run.Run(ctx, "iptables", "-D", "FORWARD", "-i", tap.InterfaceName(workspace), "-j", chain)
	_, _ = run.Run(ctx, "iptables", "-F", chain)
	_, _ = run.Run(ctx, "iptables", "-X", chain)
	_, _ = run.Run(ctx, "iptables", "-F", chain+"_N")
	_, _ = run.Run(ctx, "iptables", "-X", chain+"_N")
	return nil
}

// ClassesFor maps spec.security.egressAllow to resolved target sets,
// always including the implicit model gateway and DNS.
func ClassesFor(egressAllow []string) []string {
	classes := map[string]bool{ClassModelGateway: true, DNSClass: true}
	for _, c := range egressAllow {
		if c == ClassMemory {
			classes[ClassMemory] = true
		}
		// unknown classes are ignored (P1 vocabulary: model-gateway|memory)
	}
	out := make([]string, 0, len(classes))
	for c := range classes {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// ServiceFor returns the backing service of a class (dns included).
func ServiceFor(class string) (ServiceKey, bool) {
	if class == DNSClass {
		return dnsService, true
	}
	s, ok := ClassServices[class]
	return s, ok
}
