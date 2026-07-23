package meter

import (
	"context"
	"errors"
	"testing"

	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBatteryCapacity(t *testing.T) {
	ctx := context.TODO()

	// static value
	{
		var cc batteryCapacityCtx
		require.NoError(t, util.DecodeOther(map[string]any{"capacity": 10}, &cc))
		g, err := cc.Decorator(ctx)
		require.NoError(t, err)
		require.NotNil(t, g)
		require.Equal(t, 10.0, g())
	}

	// zero value is treated as not configured
	{
		var cc batteryCapacityCtx
		require.NoError(t, util.DecodeOther(map[string]any{"capacity": 0}, &cc))
		g, err := cc.Decorator(ctx)
		require.NoError(t, err)
		require.Nil(t, g)
	}

	// unset is not configured
	{
		var cc batteryCapacityCtx
		g, err := cc.Decorator(ctx)
		require.NoError(t, err)
		require.Nil(t, g)
	}

	// float plugin
	{
		var cc batteryCapacityCtx
		require.NoError(t, util.DecodeOther(map[string]any{
			"capacity": map[string]any{
				"source": "const",
				"value":  "12.5",
			},
		}, &cc))
		g, err := cc.Decorator(ctx)
		require.NoError(t, err)
		require.NotNil(t, g)
		require.Equal(t, 12.5, g())
	}
}

func TestBatteryPowerControllerDirection(t *testing.T) {
	var charge, discharge, stop []float64

	ctrl := &batteryPowerController{
		charge: func(power float64) error {
			charge = append(charge, power)
			return nil
		},
		discharge: func(power float64) error {
			discharge = append(discharge, power)
			return nil
		},
		stop: func(power float64) error {
			stop = append(stop, power)
			return nil
		},
	}

	require.NoError(t, ctrl.SetBatteryPower(-1000))
	require.NoError(t, ctrl.SetBatteryPower(-2000))
	require.NoError(t, ctrl.SetBatteryPower(2000))
	require.NoError(t, ctrl.SetBatteryPower(0))
	require.NoError(t, ctrl.SetBatteryPower(0))

	assert.Equal(t, []float64{1000, 2000, 0}, charge)
	assert.Equal(t, []float64{2000, 0}, discharge)
	assert.Equal(t, []float64{0, 0}, stop)

	stop = nil
	ctrl = &batteryPowerController{
		stop: func(power float64) error {
			stop = append(stop, power)
			return nil
		},
	}
	require.NoError(t, ctrl.SetBatteryPower(0))
	require.NoError(t, ctrl.SetBatteryPower(0))
	assert.Equal(t, []float64{0, 0}, stop)

	charge = nil
	discharge = nil
	ctrl = &batteryPowerController{
		charge: func(power float64) error {
			charge = append(charge, power)
			return nil
		},
		discharge: func(power float64) error {
			discharge = append(discharge, power)
			return nil
		},
	}
	require.NoError(t, ctrl.SetBatteryPower(0))
	require.NoError(t, ctrl.SetBatteryPower(0))
	assert.Equal(t, []float64{0}, charge)
	assert.Equal(t, []float64{0}, discharge)
}

func TestBatteryPowerControllerRetriesRelease(t *testing.T) {
	var charge, stop []float64
	releaseErr := errors.New("release failed")

	ctrl := &batteryPowerController{
		charge: func(power float64) error {
			charge = append(charge, power)
			return nil
		},
		stop: func(power float64) error {
			stop = append(stop, power)
			if len(stop) == 1 {
				return releaseErr
			}
			return nil
		},
	}

	require.NoError(t, ctrl.SetBatteryPower(-1000))
	require.ErrorIs(t, ctrl.SetBatteryPower(0), releaseErr)
	require.NoError(t, ctrl.SetBatteryPower(0))

	assert.Equal(t, []float64{1000, 0, 0}, charge)
	assert.Equal(t, []float64{0, 0}, stop)
}

func TestBatteryPowerControllerAttemptsReleaseAfterDirectionStopFailure(t *testing.T) {
	var charge, stop []float64
	directionErr := errors.New("direction stop failed")

	ctrl := &batteryPowerController{
		charge: func(power float64) error {
			charge = append(charge, power)
			if power == 0 {
				return directionErr
			}
			return nil
		},
		stop: func(power float64) error {
			stop = append(stop, power)
			return nil
		},
	}

	require.NoError(t, ctrl.SetBatteryPower(-1000))
	require.ErrorIs(t, ctrl.SetBatteryPower(0), directionErr)

	assert.Equal(t, []float64{1000, 0}, charge)
	assert.Equal(t, []float64{0}, stop)
	assert.Equal(t, -1, ctrl.direction)
}

func TestBatterySocLimits(t *testing.T) {
	other := map[string]any{
		"minsoc": 1,
		"maxsoc": 99,
	}

	expected := batterySocLimits{
		MinSoc: 1,
		MaxSoc: 99,
	}

	{
		var res batterySocLimits
		require.NoError(t, util.DecodeOther(other, &res))
		require.Equal(t, expected, res)
	}

	{
		var res struct {
			batterySocLimits `mapstructure:",squash"`
		}
		require.NoError(t, util.DecodeOther(other, &res))
		require.Equal(t, expected, res.batterySocLimits)
	}

	{
		var res struct {
			BatterySocLimits batterySocLimits `mapstructure:",squash"`
		}
		require.NoError(t, util.DecodeOther(other, &res))
		require.Equal(t, expected, res.BatterySocLimits)
	}

	{
		res := struct {
			batterySocLimits `mapstructure:",squash"`
		}{
			batterySocLimits: batterySocLimits{
				MinSoc: 20,
				MaxSoc: 95,
			},
		}
		require.NoError(t, util.DecodeOther(other, &res))
		require.Equal(t, expected, res.batterySocLimits)
	}

	{
		res := struct {
			pvMaxACPower     `mapstructure:",squash"`
			batterySocLimits `mapstructure:",squash"`
		}{
			batterySocLimits: batterySocLimits{
				MinSoc: 20,
				MaxSoc: 95,
			},
		}
		require.NoError(t, util.DecodeOther(other, &res))
		require.Equal(t, expected, res.batterySocLimits)
	}
}
