package iptables

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/hostcmd"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/tap"
)

func TestRulesGoldenOrder(t *testing.T) {
	rules := Rules("FC_WL_x", []Target{
		{IP: "10.42.2.9", Port: "80", Proto: "tcp"},
		{IP: "10.43.0.10", Port: "53", Proto: "both"},
		{IP: "10.42.0.49", Port: "80", Proto: "tcp"},
	})
	require.GreaterOrEqual(t, len(rules), 5)
	assert.Contains(t, rules[0], "ESTABLISHED,RELATED -j ACCEPT", "established must be first")
	assert.Contains(t, rules[len(rules)-1], "-j REJECT", "reject must be last")
	joined := strings.Join(rules, "\n")
	assert.Contains(t, joined, "-p udp -d 10.43.0.10 --dport 53")
	assert.Contains(t, joined, "-p tcp -d 10.43.0.10 --dport 53")
	// deterministic sort by IP
	assert.Less(t, strings.Index(joined, "10.42.0.49"), strings.Index(joined, "10.42.2.9"))
}

func TestRulesDedup(t *testing.T) {
	rules := Rules("C", []Target{
		{IP: "1.1.1.1", Port: "80", Proto: "tcp"},
		{IP: "1.1.1.1", Port: "80", Proto: "tcp"},
	})
	count := strings.Count(strings.Join(rules, "\n"), "-d 1.1.1.1")
	assert.Equal(t, 1, count)
}

func TestChainIDTruncation(t *testing.T) {
	long := "pw-a-very-long-workspace-name-beyond-limit"
	id := ChainID(long)
	require.LessOrEqual(t, len(id), 29, "iptables chain names cap at 29 chars")
	assert.True(t, strings.HasPrefix(id, "FC_WL_"))
}

func TestClassesForDefaults(t *testing.T) {
	assert.Equal(t, []string{"dns", "model-gateway"}, ClassesFor(nil))
	assert.Equal(t, []string{"dns", "memory", "model-gateway"}, ClassesFor([]string{"memory"}))
	// unknown vocabulary ignored (P1: model-gateway|memory)
	assert.Equal(t, []string{"dns", "model-gateway"}, ClassesFor([]string{"weird-class"}))
}

func TestEnsureEgressCommandStream(t *testing.T) {
	// -C reports "absent" (real iptables exits 1) so the -I insert path runs.
	run := &hostcmd.FakeRunner{Response: func(name string, args []string) (string, error) {
		if name == "iptables" && len(args) > 0 && args[0] == "-C" {
			return "", assert.AnError
		}
		return "", nil
	}}
	err := EnsureEgress(context.Background(), run, "ws1", []Target{{IP: "10.42.0.5", Port: "80", Proto: "tcp"}})
	require.NoError(t, err)
	calls := strings.Join(run.Calls, "\n")
	assert.Contains(t, calls, "-N FC_WL_ws1_N", "scratch chain build first")
	assert.Contains(t, calls, "-A FC_WL_ws1_N -p tcp -d 10.42.0.5 --dport 80 -j ACCEPT")
	assert.Contains(t, calls, "-F FC_WL_ws1")
	assert.Contains(t, calls, "-A FC_WL_ws1 -j REJECT")
	assert.Contains(t, calls, "-X FC_WL_ws1_N", "scratch cleanup")
	assert.Contains(t, calls, "-I FORWARD 1 -i tap-ws1 -j FC_WL_ws1")
}

func TestEnsureEgressScratchFailureKeepsLiveChain(t *testing.T) {
	// iptables -A on the scratch chain fails → no flush of the live chain.
	run := &hostcmd.FakeRunner{Response: func(name string, args []string) (string, error) {
		if name == "iptables" && len(args) >= 3 && args[0] == "-A" && args[1] == "FC_WL_ws1_N" {
			return "", assert.AnError
		}
		return "", nil
	}}
	err := EnsureEgress(context.Background(), run, "ws1", []Target{{IP: "10.42.0.5", Port: "80", Proto: "tcp"}})
	require.Error(t, err)
	assert.NotContains(t, strings.Join(run.Calls, "\n"), "-F FC_WL_ws1 ", "live chain untouched when scratch build fails")
}

func TestTeardownEgressIdempotent(t *testing.T) {
	run := &hostcmd.FakeRunner{}
	require.NoError(t, TeardownEgress(context.Background(), run, "ws1"))
	first := append([]string(nil), run.Calls...)
	require.NoError(t, TeardownEgress(context.Background(), run, "ws1"))
	assert.Equal(t, first, run.Calls[:len(first)], "second teardown replays the same absent-tolerant sequence, error-free")
}

func TestGeneratedWorkspaceUsesSameTapForForwardAndCleanup(t *testing.T) {
	run := &hostcmd.FakeRunner{Response: func(name string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "-C" {
			return "", fmt.Errorf("missing")
		}
		return "", nil
	}}
	workspace := "ws-longusername-1234"
	require.NoError(t, EnsureEgress(context.Background(), run, workspace, nil))
	require.NoError(t, TeardownEgress(context.Background(), run, workspace))
	calls := strings.Join(run.Calls, "\n")
	assert.Contains(t, calls, "-i "+tap.InterfaceName(workspace))
	assert.NotContains(t, calls, "tap-"+workspace)
}
