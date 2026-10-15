package tap

import (
	"context"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/hostcmd"
	"strings"
	"testing"
)

func TestGeneratedWorkspaceNamesFitLinuxInterfaceLimit(t *testing.T) {
	names := []string{"ws-user-74e8", "ws-longusername-1234", "ws-longusername-5678"}
	seen := map[string]bool{}
	for _, name := range names {
		dev, err := New(name, "192.168.242.0/30")
		if err != nil {
			t.Fatal(err)
		}
		if len(dev.Name) > 15 || seen[dev.Name] {
			t.Fatalf("invalid or colliding device %q", dev.Name)
		}
		seen[dev.Name] = true
		run := &hostcmd.FakeRunner{}
		_ = dev.Ensure(context.Background(), run)
		_ = dev.Teardown(context.Background(), run)
		_ = TeardownByName(context.Background(), run, name)
		calls := strings.Join(run.Calls, "\n")
		if !strings.Contains(calls, "link del "+dev.Name) || strings.Contains(calls, "tap-"+name) {
			t.Fatal(calls)
		}
	}
	if InterfaceName("cloud-qa") != "tap-cloud-qa" {
		t.Fatal("changed existing short interface")
	}
}
