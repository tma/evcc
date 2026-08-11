package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBatteryDischargeModeString(t *testing.T) {
	for _, expected := range []BatteryDischargeMode{
		BatteryDischargeAllow,
		BatteryDischargeReserve,
		BatteryDischargePrevent,
	} {
		actual, err := BatteryDischargeModeString(string(expected))
		require.NoError(t, err)
		assert.Equal(t, expected, actual)
	}

	_, err := BatteryDischargeModeString("invalid")
	require.Error(t, err)
}
