// Node assignment for microvm workspaces: the operator picks a KVM host
// from the configured pool and carves a sticky /30 out of the microvm
// subnet pool. Both are written once (status is the source of truth — a
// restart never reassigns); clearing NodeName for a lost node is a manual
// runbook action.
package controller

import (
	"context"
	"fmt"
	"net/netip"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// defaultMicrovmSubnet matches the PoC tap network (192.168.77.0/24, 64 /30s).
const defaultMicrovmSubnet = "192.168.77.0/24"

// ensureMicrovmAssignment assigns node + subnet once and marks the
// workspace Pending for the node agent. Sticky: any status with NodeName
// set is returned untouched.
func (r *PersonalWorkspaceReconciler) ensureMicrovmAssignment(ctx context.Context, workspace *v1alpha1.PersonalWorkspace) error {
	if workspace.Status.NodeName != "" {
		return nil
	}
	if len(r.MicrovmNodes) == 0 {
		return errMicrovmPoolEmpty
	}

	pool := r.MicrovmSubnet
	if pool == "" {
		pool = defaultMicrovmSubnet
	}

	var siblings v1alpha1.PersonalWorkspaceList
	if err := r.List(ctx, &siblings); err != nil {
		return errWrap("list workspaces for assignment: ", err)
	}
	usedSubnets := map[string]bool{}
	perNode := map[string]int{}
	for i := range siblings.Items {
		ws := &siblings.Items[i]
		if ws.Status.VMSubnet != "" {
			usedSubnets[ws.Status.VMSubnet] = true
		}
		if ws.Status.NodeName != "" {
			perNode[ws.Status.NodeName]++
		}
	}

	subnet, err := firstFreeSubnet(pool, usedSubnets)
	if err != nil {
		return errWrap("microvm subnet pool: ", err)
	}
	node := leastAssigned(r.MicrovmNodes, perNode)

	old := workspace.DeepCopy()
	workspace.Status.NodeName = node
	workspace.Status.VMSubnet = subnet
	workspace.Status.Phase = v1alpha1.WorkspacePhasePending
	workspace.Status.ObservedGeneration = workspace.Generation
	workspace.Status.Message = ""
	// Merge patch (not Update): the node agent may concurrently add its
	// finalizer or report status — each writer only touches its own fields.
	if err := r.Status().Patch(ctx, workspace, client.MergeFrom(old)); err != nil {
		return errWrap("assign microvm node: ", err)
	}
	log.FromContext(ctx).Info("microvm workspace assigned", "node", node, "subnet", subnet)
	return nil
}

// leastAssigned picks the pool node with the fewest assigned workspaces;
// ties keep pool order (the cicd pool lists the preferred host first, so
// the first workspace lands there).
func leastAssigned(pool []string, perNode map[string]int) string {
	best := pool[0]
	bestCount := -1
	for _, node := range pool {
		count := perNode[node]
		if bestCount < 0 || count < bestCount {
			best, bestCount = node, count
		}
	}
	return best
}

// firstFreeSubnet returns the lowest /30 of the /24 pool not in used.
func firstFreeSubnet(poolCIDR string, used map[string]bool) (string, error) {
	base, err := netip.ParsePrefix(poolCIDR)
	if err != nil || !base.Addr().Is4() || base.Bits() != 24 {
		return "", fmt.Errorf("pool %q is not an IPv4 /24", poolCIDR)
	}
	octets := base.Addr().As4()
	for i := 0; i < 64; i++ {
		candidate := netip.PrefixFrom(netip.AddrFrom4(octets), 30).String()
		if !used[candidate] {
			return candidate, nil
		}
		octets[3] += 4
	}
	return "", fmt.Errorf("microvm subnet pool %s exhausted (64 /30s in use)", poolCIDR)
}
