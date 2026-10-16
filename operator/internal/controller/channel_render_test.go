package controller

import (
	"encoding/json"
	"testing"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

func TestRenderWechatChannelGateWithoutCredentials(t *testing.T) {
	employee := &dev1alpha1.DigitalEmployee{}
	employee.Name = "hr-bot"
	employee.Spec.Channels = &dev1alpha1.ChannelsSpec{Wechat: true}
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(renderConfig(employee, "")), &result))
	channels, ok := result["channel_connections"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, channels["enabled"])
	require.Equal(t, false, channels["require_bound_identity"])
	require.Equal(t, map[string]any{"enabled": true}, channels["wechat"])
	require.NotContains(t, channels, "wecom")
	require.NotContains(t, channels, "feishu")
	require.NotContains(t, result, "bot_token")
}
