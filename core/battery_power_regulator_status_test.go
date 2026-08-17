package core

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/tariff"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type publishedBatteryPowerControl struct {
	values []batteryPowerControlStatus
}

func (p *publishedBatteryPowerControl) publish(val any) {
	p.values = append(p.values, val.(batteryPowerControlStatus))
}

func (p *publishedBatteryPowerControl) last() batteryPowerControlStatus {
	return p.values[len(p.values)-1]
}

func newStatusTestRegulator(t *testing.T, gridPower, batteryPower float64) (*batteryPowerRegulator, *clock.Mock, *regulatorTestMeter, *publishedBatteryPowerControl) {
	t.Helper()

	grid := &regulatorTestMeter{power: gridPower}
	batteryMeter := &regulatorTestMeter{power: batteryPower}
	controller := &regulatorTestController{}
	var battery api.Meter = &struct {
		api.Meter
		api.BatteryPowerController
		api.BatteryPowerLimiter
	}{
		Meter:                  batteryMeter,
		BatteryPowerController: controller,
		BatteryPowerLimiter:    testBatteryPowerLimiter{charge: 5000, discharge: 5000},
	}
	regulator := newBatteryPowerRegulator(util.NewLogger(t.Name()), grid, []config.Device[api.Meter]{
		config.NewStaticDevice(config.Named{Name: "battery"}, battery),
	})
	require.NotNil(t, regulator)
	clck := clock.NewMock()
	regulator.clock = clck

	published := &publishedBatteryPowerControl{}
	regulator.setPublisher(published.publish)
	return regulator, clck, grid, published
}

func TestBatteryPowerRegulatorPublishesStatus(t *testing.T) {
	regulator, clck, grid, published := newStatusTestRegulator(t, 4500, 0)

	require.NoError(t, regulator.setPolicy(batteryPowerControlPolicy{
		valid:            true,
		active:           true,
		chargeAllowed:    true,
		dischargeAllowed: true,
		residualPower:    100,
		chargeLimit:      5000,
		dischargeLimit:   5000,
	}))
	require.Len(t, published.values, 1)
	acquired := published.last()
	assert.Equal(t, "neutral", acquired.Phase)
	assert.Equal(t, 0.0, acquired.Command)
	assert.Nil(t, acquired.Pending)
	assert.Equal(t, "acquired control", acquired.Reason)
	assert.True(t, acquired.Initialized)
	assert.True(t, acquired.NeutralRequired)

	clck.Add(batteryPowerControlInterval)
	regulator.tick()
	require.Len(t, published.values, 2)
	commanded := published.last()
	assert.Equal(t, "discharging", commanded.Phase)
	assert.Greater(t, commanded.Command, 0.0)
	require.NotNil(t, commanded.Pending)
	assert.Equal(t, 0.0, commanded.Pending.Previous)
	assert.Equal(t, commanded.Command, commanded.Pending.Command)
	assert.False(t, commanded.Pending.AppliedAt.IsZero())

	payload, err := json.Marshal(commanded)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(payload, &decoded))
	assert.Equal(t, "discharging", decoded["phase"])
	assert.Contains(t, decoded, "pending")
	assert.Contains(t, decoded, "command")
	assert.NotContains(t, decoded, "chargeBlockedUntil")
	assert.NotContains(t, decoded, "dischargeBlockedUntil")

	unchanged := len(published.values)
	clck.Add(batteryPowerControlInterval)
	regulator.tick()
	assert.Equal(t, unchanged, len(published.values), "unchanged tick must not republish")

	commanded.Pending.Command = 999
	commanded.Command = 888
	assert.NotEqual(t, 999.0, regulator.pendingCommand.Command)
	assert.NotEqual(t, 888.0, regulator.appliedCommand)

	grid.set(0, errors.New("grid down"))
	clck.Add(batteryPowerControlInterval)
	regulator.tick()
	require.Greater(t, len(published.values), unchanged)
	faulted := published.last()
	assert.Equal(t, "faultStopping", faulted.Phase)
	assert.Equal(t, "grid unavailable", faulted.Reason)

	require.NoError(t, regulator.release())
	released := published.last()
	assert.Equal(t, "released", released.Phase)
	assert.Equal(t, 0.0, released.Command)
	assert.Nil(t, released.Pending)
	assert.Equal(t, "released", released.Reason)
}

func TestBatteryPowerRegulatorPublishesLoadpointDemandRetreat(t *testing.T) {
	regulator, clck, _, published := newStatusTestRegulator(t, -2000, -3000)
	regulator.phase = batteryPowerCharging
	regulator.appliedCommand = -3000
	regulator.initialized = true

	require.NoError(t, regulator.setLoadpointDemand(1380, clck.Now().Add(time.Minute)))
	require.NotEmpty(t, published.values)
	status := published.last()
	assert.Equal(t, "charging", status.Phase)
	assert.Equal(t, -1620.0, status.Command)
	assert.Equal(t, "loadpoint demand feed-forward", status.Reason)
}

func TestBatteryPowerRegulatorStatusIsCopy(t *testing.T) {
	regulator, clck, _, _ := newStatusTestRegulator(t, 300, 0)
	now := clck.Now()
	regulator.appliedCommand = 1500
	regulator.initialized = true
	regulator.phase = batteryPowerDischarging
	regulator.lastCommandReason = "test"
	regulator.pendingCommand = &pendingBatteryPowerCommand{
		PreviousCommand: 0,
		Command:         1500,
		AppliedAt:       now,
	}
	blocked := now.Add(time.Minute)
	regulator.dischargeBlockedUntil = blocked

	status := regulator.status()
	require.NotNil(t, status.Pending)
	status.Pending.Command = 1
	status.Pending.Previous = 2
	status.Command = 3
	*status.DischargeBlockedUntil = now

	assert.Equal(t, 1500.0, regulator.pendingCommand.Command)
	assert.Equal(t, 0.0, regulator.pendingCommand.PreviousCommand)
	assert.Equal(t, 1500.0, regulator.appliedCommand)
	assert.Equal(t, blocked, regulator.dischargeBlockedUntil)
}

func TestSitePreparePublishesBatteryPowerControl(t *testing.T) {
	t.Run("no regulator", func(t *testing.T) {
		valueChan := make(chan util.Param, 64)
		site := &Site{
			log:       util.NewLogger(t.Name()),
			valueChan: valueChan,
			tariffs:   &tariff.Tariffs{},
		}

		site.prepare()

		assert.Nil(t, receiveKey(t, valueChan, keys.BatteryPowerControl))
	})

	t.Run("released regulator", func(t *testing.T) {
		regulator, _, _, _ := newStatusTestRegulator(t, 0, 0)
		valueChan := make(chan util.Param, 64)
		site := &Site{
			log:                   util.NewLogger(t.Name()),
			valueChan:             valueChan,
			tariffs:               &tariff.Tariffs{},
			batteryPowerRegulator: regulator,
		}

		site.prepare()

		status := receiveKey(t, valueChan, keys.BatteryPowerControl).(batteryPowerControlStatus)
		assert.Equal(t, "released", status.Phase)
		assert.Equal(t, 0.0, status.Command)
		assert.Nil(t, status.Pending)
		assert.False(t, status.Initialized)
	})
}

func TestBatteryPowerRegulatorPublishesThroughSite(t *testing.T) {
	valueChan := make(chan util.Param, 8)
	site := &Site{
		log:       util.NewLogger(t.Name()),
		valueChan: valueChan,
	}
	regulator, _, _, _ := newStatusTestRegulator(t, 0, 0)
	regulator.setPublisher(func(val any) {
		site.publish(keys.BatteryPowerControl, val)
	})

	require.NoError(t, regulator.setPolicy(batteryPowerControlPolicy{
		valid:            true,
		active:           true,
		chargeAllowed:    true,
		dischargeAllowed: true,
		chargeLimit:      5000,
		dischargeLimit:   5000,
	}))

	param := receiveKey(t, valueChan, keys.BatteryPowerControl)
	status := param.(batteryPowerControlStatus)
	assert.Equal(t, "neutral", status.Phase)
}

func receiveKey(t *testing.T, ch <-chan util.Param, key string) any {
	t.Helper()

	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case param := <-ch:
			if param.Key == key {
				return param.Val
			}
		case <-timer.C:
			t.Fatalf("timeout waiting for %q", key)
		}
	}
}
