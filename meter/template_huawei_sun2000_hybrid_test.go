package meter

import (
	"strings"
	"testing"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/plugin"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/templates"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

func TestHuaweiContinuousBatteryControlTemplate(t *testing.T) {
	instance, err := templates.RenderInstance(templates.Meter, map[string]any{
		"template":          "huawei-sun2000-hybrid",
		"usage":             "battery",
		"continuousControl": true,
		"storageunit":       "1",
		"maxchargepower":    "5000",
		"maxdischargepower": "5000",
		"modbus":            "tcpip",
		"host":              "192.0.2.2",
		"port":              "502",
		"id":                "1",
	})
	require.NoError(t, err)

	var decoded batteryPowerControlConfig
	require.NoError(t, util.DecodeOther(instance.Other["batterypower"], &decoded))
	require.NotNil(t, decoded.Charge)
	require.NotNil(t, decoded.Discharge)
	require.NotNil(t, decoded.Stop)
	testHuaweiPowerClampScript(t, decoded.Charge)
	testHuaweiPowerClampScript(t, decoded.Discharge)

	config, err := yaml.Marshal(instance)
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(config), "timeout: 10s"))

	meter, err := NewFromConfig(t.Context(), instance.Type, instance.Other)
	require.NoError(t, err)
	require.True(t, api.HasCap[api.BatteryPowerController](meter))
}

func testHuaweiPowerClampScript(t *testing.T, control *plugin.Config) {
	t.Helper()

	require.Equal(t, "sequence", control.Source)

	var sequence struct {
		Set []plugin.Config
	}
	require.NoError(t, util.DecodeOther(control.Other, &sequence))
	require.NotEmpty(t, sequence.Set)
	require.Equal(t, "go", sequence.Set[0].Source)

	script, ok := sequence.Set[0].Other["script"].(string)
	require.True(t, ok)

	provider, err := plugin.NewGoPluginFromConfig(t.Context(), map[string]any{
		"in": []any{
			map[string]any{
				"name": "rated",
				"type": "int",
				"config": map[string]any{
					"source": "const",
					"value":  "12000",
				},
			},
		},
		"out": []any{
			map[string]any{
				"name": "maxPower",
				"type": "int",
				"config": map[string]any{
					"source":   "sleep",
					"duration": "0s",
				},
			},
			map[string]any{
				"name": "power",
				"type": "int",
				"config": map[string]any{
					"source":   "sleep",
					"duration": "0s",
				},
			},
		},
		"script": script,
	})
	require.NoError(t, err)

	setter, ok := provider.(plugin.IntSetter)
	require.True(t, ok)
	set, err := setter.IntSetter("power")
	require.NoError(t, err)
	require.NoError(t, set(4359))
}
