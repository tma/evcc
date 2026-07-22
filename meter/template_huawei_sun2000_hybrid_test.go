package meter

import (
	"strings"
	"testing"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util/templates"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

func TestHuaweiContinuousBatteryControlTemplate(t *testing.T) {
	instance, err := templates.RenderInstance(templates.Meter, map[string]any{
		"template":          "huawei-sun2000-hybrid",
		"usage":             "battery",
		"continuousControl": "true",
		"storageunit":       "1",
		"maxchargepower":    "5000",
		"maxdischargepower": "5000",
		"modbus":            "tcpip",
		"host":              "192.0.2.2",
		"port":              "502",
		"id":                "1",
	})
	require.NoError(t, err)

	config, err := yaml.Marshal(instance)
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(config), "timeout: 10s"))

	meter, err := NewFromConfig(t.Context(), instance.Type, instance.Other)
	require.NoError(t, err)
	require.True(t, api.HasCap[api.BatteryPowerController](meter))
}
