package meter

import (
	"strings"
	"testing"
	"time"

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
	require.NotNil(t, decoded.ChargeUpdate)
	require.NotNil(t, decoded.Discharge)
	require.NotNil(t, decoded.DischargeUpdate)
	require.NotNil(t, decoded.Stop)
	require.Equal(t, 30*time.Second, decoded.Refresh)
	testHuaweiPowerControlSequence(t, decoded.Charge)
	testHuaweiPowerControlSequence(t, decoded.Discharge)
	testHuaweiPowerUpdate(t, decoded.ChargeUpdate, 47247)
	testHuaweiPowerUpdate(t, decoded.DischargeUpdate, 47249)
	testHuaweiPowerStopSequence(t, decoded.Stop)

	config, err := yaml.Marshal(instance)
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(config), "timeout: 10s"))

	meter, err := NewFromConfig(t.Context(), instance.Type, instance.Other)
	require.NoError(t, err)
	require.True(t, api.HasCap[api.BatteryPowerController](meter))
}

func testHuaweiPowerControlSequence(t *testing.T, control *plugin.Config) {
	t.Helper()

	require.Equal(t, "sequence", control.Source)

	var sequence struct {
		Set []plugin.Config
	}
	require.NoError(t, util.DecodeOther(control.Other, &sequence))
	require.Len(t, sequence.Set, 5)

	for i := range 4 {
		require.Equal(t, "ifelse", sequence.Set[i].Source, "step %d", i)

		var wrapped struct {
			If   plugin.Config
			Else plugin.Config
		}
		require.NoError(t, util.DecodeOther(sequence.Set[i].Other, &wrapped))
		require.Equal(t, "sleep", wrapped.Else.Source, "step %d else branch must be a no-op", i)
	}

	require.Equal(t, "watchdog", sequence.Set[4].Source)

	var ceiling struct {
		If   plugin.Config
		Else plugin.Config
	}
	require.NoError(t, util.DecodeOther(sequence.Set[0].Other, &ceiling))
	ceilingScript, ok := ceiling.If.Other["script"].(string)
	require.True(t, ok)
	assert.Contains(t, ceilingScript, "out := rated")
	assert.NotContains(t, ceilingScript, "requested := power")

	var ifelse struct {
		If   plugin.Config
		Else plugin.Config
	}
	require.NoError(t, util.DecodeOther(sequence.Set[1].Other, &ifelse))
	require.Equal(t, "go", ifelse.If.Source)

	script, ok := ifelse.If.Other["script"].(string)
	require.True(t, ok)
	assert.Contains(t, script, "requested := power")
	testHuaweiPowerScript(t, script)
}

func testHuaweiPowerUpdate(t *testing.T, control *plugin.Config, address int) {
	t.Helper()

	require.Equal(t, "go", control.Source)

	var update struct {
		In  []any
		Out []struct {
			Name   string
			Type   string
			Config plugin.Config
		}
		Script string
	}
	require.NoError(t, util.DecodeOther(control.Other, &update))
	require.Len(t, update.Out, 1)
	require.Equal(t, "modbus", update.Out[0].Config.Source)

	register, ok := update.Out[0].Config.Other["register"].(map[string]any)
	require.True(t, ok)
	require.EqualValues(t, address, register["address"])

	assert.Contains(t, update.Script, "requested := power")
	testHuaweiPowerScript(t, update.Script)
}

func testHuaweiPowerScript(t *testing.T, script string) {
	t.Helper()

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

func testHuaweiPowerStopSequence(t *testing.T, control *plugin.Config) {
	t.Helper()

	require.Equal(t, "sequence", control.Source)

	var sequence struct {
		Set []plugin.Config
	}
	require.NoError(t, util.DecodeOther(control.Other, &sequence))
	require.Len(t, sequence.Set, 3)
	require.Equal(t, "const", sequence.Set[0].Source)
	require.Equal(t, "go", sequence.Set[1].Source)
	require.Equal(t, "go", sequence.Set[2].Source)
}

func TestHuaweiWatchdogStopSkipsPreconditions(t *testing.T) {
	var cfg plugin.Config
	require.NoError(t, util.DecodeOther(map[string]any{
		"source": "sequence",
		"set": []any{
			map[string]any{
				"source": "ifelse",
				"if": map[string]any{
					"source": "error",
					"error":  "ErrMustRetry",
				},
				"else": map[string]any{
					"source":   "sleep",
					"duration": "0s",
				},
			},
			map[string]any{
				"source":  "watchdog",
				"timeout": "1m",
				"reset":   []string{"0"},
				"set": map[string]any{
					"source": "error",
					"error":  "ErrNotAvailable",
				},
			},
		},
	}, &cfg))

	set, err := cfg.IntSetter(t.Context(), "power")
	require.NoError(t, err)

	err = set(0)
	require.ErrorIs(t, err, api.ErrNotAvailable)

	err = set(5000)
	require.ErrorIs(t, err, api.ErrMustRetry)
}
