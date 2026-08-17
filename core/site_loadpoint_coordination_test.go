package core

import (
	"errors"
	"sync"
	"testing"
	"time"

	evbus "github.com/asaskevich/EventBus"
	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

type demandTestMeter struct {
	power    float64
	currents []float64
}

type partialSuccessCharger struct {
	appliedPower float64
	enableErr    error
	writes       int
}

func (c *partialSuccessCharger) Status() (api.ChargeStatus, error) {
	return api.StatusB, nil
}

func (c *partialSuccessCharger) Enabled() (bool, error) {
	return false, nil
}

func (c *partialSuccessCharger) Enable(enable bool) error {
	if enable {
		c.writes++
	}
	return c.enableErr
}

func (c *partialSuccessCharger) MaxCurrent(current int64) error {
	return c.MaxCurrentMillis(float64(current))
}

func (c *partialSuccessCharger) MaxCurrentMillis(current float64) error {
	c.appliedPower = current * Voltage
	c.writes++
	return nil
}

func (m *demandTestMeter) CurrentPower() (float64, error) {
	return m.power, nil
}

func (m *demandTestMeter) Currents() (float64, float64, float64, error) {
	return m.currents[0], m.currents[1], m.currents[2], nil
}

func newDemandTestLoadpoint(t *testing.T, site *Site, charger api.Charger, clck *clock.Mock) *Loadpoint {
	t.Helper()

	return &Loadpoint{
		log:            util.NewLogger(t.Name()),
		bus:            evbus.New(),
		clock:          clck,
		site:           site,
		charger:        charger,
		wakeUpTimer:    NewTimer(),
		minCurrent:     minA,
		maxCurrent:     maxA,
		phases:         1,
		status:         api.StatusB,
		mode:           api.ModePV,
		chargePower:    0,
		offeredCurrent: 0,
	}
}

func TestLoadpointDemandObservationPreservesMeasuredPower(t *testing.T) {
	Voltage = 230
	meter := &demandTestMeter{
		power:    1000,
		currents: []float64{2, 2, 2},
	}
	lp := &Loadpoint{
		log:         util.NewLogger(t.Name()),
		clock:       clock.NewMock(),
		chargeMeter: meter,
	}

	assert.Equal(t, 1000.0, lp.UpdateChargePowerAndCurrents())
	power, valid := lp.physicalDemandPower()
	assert.True(t, valid)
	assert.Equal(t, 1380.0, power)
}

func TestLoadpointDemandPreparedBeforeCommand(t *testing.T) {
	Voltage = 230

	t.Run("enable", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		charger := api.NewMockCharger(ctrl)
		clck := clock.NewMock()
		site := &Site{log: util.NewLogger(t.Name())}
		site.initLoadpointCoordination(30 * time.Second)
		lp := newDemandTestLoadpoint(t, site, charger, clck)

		charger.EXPECT().MaxCurrent(int64(minA)).Return(nil)
		charger.EXPECT().Enable(true).DoAndReturn(func(bool) error {
			assert.Equal(t, currentToPower(minA, 1), site.pendingLoadpointDemand(lp, clck.Now()))
			return nil
		})

		require.NoError(t, lp.setLimit(minA))
		assert.Equal(t, currentToPower(minA, 1), site.pendingLoadpointDemand(lp, clck.Now()))
	})

	t.Run("current increase", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		charger := api.NewMockCharger(ctrl)
		clck := clock.NewMock()
		site := &Site{log: util.NewLogger(t.Name())}
		site.initLoadpointCoordination(30 * time.Second)
		lp := newDemandTestLoadpoint(t, site, charger, clck)
		lp.enabled = true
		lp.status = api.StatusC
		lp.offeredCurrent = minA
		lp.chargePower = currentToPower(minA, 1)
		lp.demandPower = lp.chargePower
		lp.demandValid = true

		charger.EXPECT().MaxCurrent(int64(maxA)).DoAndReturn(func(int64) error {
			assert.Equal(t, currentToPower(maxA-minA, 1), site.pendingLoadpointDemand(lp, clck.Now()))
			return nil
		})

		require.NoError(t, lp.setLimit(maxA))
		assert.Equal(t, currentToPower(maxA-minA, 1), site.pendingLoadpointDemand(lp, clck.Now()))
	})

	t.Run("meterless increase uses command delta", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		charger := api.NewMockCharger(ctrl)
		clck := clock.NewMock()
		site := &Site{log: util.NewLogger(t.Name())}
		site.initLoadpointCoordination(30 * time.Second)
		lp := newDemandTestLoadpoint(t, site, charger, clck)
		lp.enabled = true
		lp.status = api.StatusC
		lp.offeredCurrent = minA
		lp.chargePower = currentToPower(minA, 1)

		charger.EXPECT().MaxCurrent(int64(minA + 1)).Return(nil)

		require.NoError(t, lp.setLimit(minA+1))
		assert.Equal(t, currentToPower(1, 1), site.pendingLoadpointDemand(lp, clck.Now()))
	})

	t.Run("meterless increase retains earlier claim", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		charger := api.NewMockCharger(ctrl)
		clck := clock.NewMock()
		site := &Site{log: util.NewLogger(t.Name())}
		site.initLoadpointCoordination(30 * time.Second)
		lp := newDemandTestLoadpoint(t, site, charger, clck)

		charger.EXPECT().MaxCurrent(int64(minA)).Return(nil)
		charger.EXPECT().Enable(true).Return(nil)
		require.NoError(t, lp.setLimit(minA))
		assert.Equal(t, currentToPower(minA, 1), site.pendingLoadpointDemand(lp, clck.Now()))

		charger.EXPECT().MaxCurrent(int64(minA + 1)).Return(nil)
		require.NoError(t, lp.setLimit(minA+1))
		assert.Equal(t, currentToPower(minA+1, 1), site.pendingLoadpointDemand(lp, clck.Now()))
	})

	t.Run("failed physical sample uses command delta", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		charger := api.NewMockCharger(ctrl)
		clck := clock.NewMock()
		site := &Site{log: util.NewLogger(t.Name())}
		site.initLoadpointCoordination(30 * time.Second)
		lp := newDemandTestLoadpoint(t, site, charger, clck)
		lp.phases = 3
		lp.enabled = true
		lp.status = api.StatusC
		lp.offeredCurrent = maxA - 1
		lp.chargeMeter = &demandTestMeter{}
		lp.demandPower = currentToPower(maxA-1, 3)
		lp.demandValid = false

		now := clck.Now()
		_, err := site.prepareLoadpointDemand(lp, 500, 0, now, now.Add(time.Minute))
		require.NoError(t, err)
		site.commitLoadpointDemand(lp)

		expected := 500 + currentToPower(1, 3)
		charger.EXPECT().MaxCurrent(int64(maxA)).Return(nil)
		require.NoError(t, lp.setLimit(maxA))
		assert.Equal(t, expected, site.pendingLoadpointDemand(lp, clck.Now()))

		require.NoError(t, site.observeLoadpointDemand(lp, currentToPower(maxA, 3), false, clck.Now()))
		assert.Equal(t, expected, site.pendingLoadpointDemand(lp, clck.Now()), "invalid sample cannot acknowledge")

		require.NoError(t, site.observeLoadpointDemand(lp, currentToPower(maxA, 3), true, clck.Now()))
		assert.Zero(t, site.pendingLoadpointDemand(lp, clck.Now()))
	})

	t.Run("partial actuator success retains claim", func(t *testing.T) {
		clck := clock.NewMock()
		charger := &partialSuccessCharger{enableErr: errors.New("enable write failed")}
		site := &Site{log: util.NewLogger(t.Name())}
		site.initLoadpointCoordination(30 * time.Second)
		lp := newDemandTestLoadpoint(t, site, charger, clck)

		require.Error(t, lp.setLimit(minA))
		assert.Equal(t, currentToPower(minA, 1), charger.appliedPower)
		assert.Equal(t, 2, charger.writes, "current and enable both reached the actuator")
		assert.Equal(t, currentToPower(minA, 1), site.pendingLoadpointDemand(lp, clck.Now()))

		charger.enableErr = nil
		require.NoError(t, lp.setLimit(minA))
		assert.Equal(t, 3, charger.writes, "retry only repeats enable")
		assert.Equal(t, currentToPower(minA, 1), site.pendingLoadpointDemand(lp, clck.Now()))
	})

	t.Run("failure restores previous claim", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		charger := api.NewMockCharger(ctrl)
		clck := clock.NewMock()
		site := &Site{log: util.NewLogger(t.Name())}
		site.initLoadpointCoordination(30 * time.Second)
		lp := newDemandTestLoadpoint(t, site, charger, clck)
		lp.enabled = true
		lp.status = api.StatusC
		lp.offeredCurrent = minA

		now := clck.Now()
		_, err := site.prepareLoadpointDemand(lp, currentToPower(minA, 1), 0, now, now.Add(time.Minute))
		require.NoError(t, err)
		site.commitLoadpointDemand(lp)

		charger.EXPECT().MaxCurrent(int64(maxA)).Return(errors.New("write failed"))
		require.Error(t, lp.setLimit(maxA))
		assert.Equal(t, currentToPower(minA, 1), site.pendingLoadpointDemand(lp, clck.Now()))
	})

	t.Run("decrease retains unmet physical demand", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		charger := api.NewMockCharger(ctrl)
		clck := clock.NewMock()
		site := &Site{log: util.NewLogger(t.Name())}
		site.initLoadpointCoordination(30 * time.Second)
		lp := newDemandTestLoadpoint(t, site, charger, clck)
		lp.enabled = true
		lp.status = api.StatusC
		lp.offeredCurrent = maxA
		lp.demandPower = currentToPower(minA, 1)
		lp.demandValid = true

		now := clck.Now()
		_, err := site.prepareLoadpointDemand(
			lp,
			currentToPower(maxA, 1),
			lp.demandPower,
			now,
			now.Add(time.Minute),
		)
		require.NoError(t, err)
		site.commitLoadpointDemand(lp)

		charger.EXPECT().MaxCurrent(int64(10)).Return(nil)
		require.NoError(t, lp.setLimit(10))
		assert.Equal(t, currentToPower(10-minA, 1), site.pendingLoadpointDemand(lp, clck.Now()))
	})

	t.Run("decrease releases physically met claim", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		charger := api.NewMockCharger(ctrl)
		clck := clock.NewMock()
		site := &Site{log: util.NewLogger(t.Name())}
		site.initLoadpointCoordination(30 * time.Second)
		lp := newDemandTestLoadpoint(t, site, charger, clck)
		lp.enabled = true
		lp.status = api.StatusC
		lp.offeredCurrent = maxA
		lp.demandPower = currentToPower(10, 1)
		lp.demandValid = true

		now := clck.Now()
		_, err := site.prepareLoadpointDemand(lp, currentToPower(maxA, 1), lp.demandPower, now, now.Add(time.Minute))
		require.NoError(t, err)
		site.commitLoadpointDemand(lp)

		charger.EXPECT().MaxCurrent(int64(10)).Return(nil)
		require.NoError(t, lp.setLimit(10))
		assert.Zero(t, site.pendingLoadpointDemand(lp, clck.Now()))
	})

	t.Run("decrease without claim does not schedule dependents", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		charger := api.NewMockCharger(ctrl)
		clck := clock.NewMock()
		site := &Site{log: util.NewLogger(t.Name())}
		site.initLoadpointCoordination(30 * time.Second)
		lp := newDemandTestLoadpoint(t, site, charger, clck)
		lp.priority = 1
		lp.enabled = true
		lp.status = api.StatusC
		lp.offeredCurrent = maxA
		low := newPVLoadpoint(0, api.ModePV, api.StatusC, true, time.Time{})
		site.loadpoints = []*Loadpoint{lp, low}

		charger.EXPECT().MaxCurrent(int64(minA)).Return(nil)
		require.NoError(t, lp.setLimit(minA))
		assert.Nil(t, site.nextLoadpointReevaluation())
	})
}

func TestLoadpointDemandPreparedBeforePhaseSwitch(t *testing.T) {
	Voltage = 230

	t.Run("metered unchanged current", func(t *testing.T) {
		f := newRegulatorTestFixture(t, -5000, -4200, 0)
		f.regulator.phase = batteryPowerCharging
		f.regulator.appliedCommand = -4200
		f.regulator.initialized = true
		f.controller.reset()

		ctrl := gomock.NewController(t)
		plainCharger := api.NewMockCharger(ctrl)
		phaseCharger := api.NewMockPhaseSwitcher(ctrl)
		charger := struct {
			*api.MockCharger
			*api.MockPhaseSwitcher
		}{plainCharger, phaseCharger}
		site := &Site{
			log:                   util.NewLogger(t.Name()),
			batteryPowerRegulator: f.regulator,
		}
		site.initLoadpointCoordination(30 * time.Second)
		lp := newDemandTestLoadpoint(t, site, charger, f.clock)
		lp.enabled = true
		lp.status = api.StatusC
		lp.offeredCurrent = minA
		lp.chargeMeter = &demandTestMeter{}
		lp.demandPower = currentToPower(minA, 1)
		lp.demandValid = true

		expected := currentToPower(minA, 2)
		phaseCharger.EXPECT().Phases1p3p(3).DoAndReturn(func(int) error {
			assert.Equal(t, expected, site.pendingLoadpointDemand(lp, f.clock.Now()))
			assert.Equal(t, []float64{-1440}, f.controller.values(), "battery retreat precedes phase write")
			return nil
		})

		require.NoError(t, lp.scalePhases(3))
		require.NoError(t, lp.setLimit(minA))
		assert.Equal(t, expected, site.pendingLoadpointDemand(lp, f.clock.Now()))
	})

	t.Run("partial acknowledgement then current increase", func(t *testing.T) {
		f := newRegulatorTestFixture(t, -5000, -4200, 0)
		f.regulator.phase = batteryPowerCharging
		f.regulator.appliedCommand = -4200
		f.regulator.initialized = true
		f.controller.reset()

		ctrl := gomock.NewController(t)
		plainCharger := api.NewMockCharger(ctrl)
		phaseCharger := api.NewMockPhaseSwitcher(ctrl)
		charger := struct {
			*api.MockCharger
			*api.MockPhaseSwitcher
		}{plainCharger, phaseCharger}
		site := &Site{
			log:                   util.NewLogger(t.Name()),
			batteryPowerRegulator: f.regulator,
		}
		site.initLoadpointCoordination(30 * time.Second)
		lp := newDemandTestLoadpoint(t, site, charger, f.clock)
		lp.enabled = true
		lp.status = api.StatusC
		lp.offeredCurrent = minA
		lp.chargeMeter = &demandTestMeter{}
		lp.demandPower = currentToPower(minA, 1)
		lp.demandValid = true

		phaseCharger.EXPECT().Phases1p3p(3).DoAndReturn(func(int) error {
			assert.Equal(t, []float64{-1440}, f.controller.values())
			return nil
		})
		require.NoError(t, lp.scalePhases(3))

		observed := currentToPower(minA, 3) - 690
		lp.demandPower = observed
		require.NoError(t, site.observeLoadpointDemand(lp, observed, true, f.clock.Now()))
		assert.Equal(t, 690.0, site.pendingLoadpointDemand(lp, f.clock.Now()))
		assert.Equal(t, []float64{-1440}, f.controller.values(), "acknowledgement does not increase charging")

		plainCharger.EXPECT().MaxCurrent(int64(minA + 1)).DoAndReturn(func(int64) error {
			assert.Equal(t, 1380.0, site.pendingLoadpointDemand(lp, f.clock.Now()))
			assert.Equal(t, []float64{-1440, -750}, f.controller.values(), "new outstanding demand retreats incrementally")
			return nil
		})
		require.NoError(t, lp.setLimit(minA+1))
	})

	t.Run("meterless combined phase and current increase", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		plainCharger := api.NewMockCharger(ctrl)
		phaseCharger := api.NewMockPhaseSwitcher(ctrl)
		charger := struct {
			*api.MockCharger
			*api.MockPhaseSwitcher
		}{plainCharger, phaseCharger}
		clck := clock.NewMock()
		site := &Site{log: util.NewLogger(t.Name())}
		site.initLoadpointCoordination(30 * time.Second)
		lp := newDemandTestLoadpoint(t, site, charger, clck)
		lp.enabled = true
		lp.status = api.StatusC
		lp.offeredCurrent = 10

		phaseClaim := currentToPower(10, 3) - currentToPower(10, 1)
		phaseCharger.EXPECT().Phases1p3p(3).DoAndReturn(func(int) error {
			assert.Equal(t, phaseClaim, site.pendingLoadpointDemand(lp, clck.Now()))
			return nil
		})
		require.NoError(t, lp.scalePhases(3))

		combinedClaim := currentToPower(maxA, 3) - currentToPower(10, 1)
		plainCharger.EXPECT().MaxCurrent(int64(maxA)).DoAndReturn(func(int64) error {
			assert.Equal(t, combinedClaim, site.pendingLoadpointDemand(lp, clck.Now()))
			return nil
		})
		require.NoError(t, lp.setLimit(maxA))
		assert.Equal(t, combinedClaim, site.pendingLoadpointDemand(lp, clck.Now()))
	})

	t.Run("failure restores previous claim", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		plainCharger := api.NewMockCharger(ctrl)
		phaseCharger := api.NewMockPhaseSwitcher(ctrl)
		charger := struct {
			*api.MockCharger
			*api.MockPhaseSwitcher
		}{plainCharger, phaseCharger}
		clck := clock.NewMock()
		site := &Site{log: util.NewLogger(t.Name())}
		site.initLoadpointCoordination(30 * time.Second)
		lp := newDemandTestLoadpoint(t, site, charger, clck)
		lp.enabled = true
		lp.status = api.StatusC
		lp.offeredCurrent = minA

		now := clck.Now()
		_, err := site.prepareLoadpointDemand(lp, 500, 0, now, now.Add(time.Minute))
		require.NoError(t, err)
		site.commitLoadpointDemand(lp)

		expectedPrepared := 500 + currentToPower(minA, 2)
		phaseCharger.EXPECT().Phases1p3p(3).DoAndReturn(func(int) error {
			assert.Equal(t, expectedPrepared, site.pendingLoadpointDemand(lp, clck.Now()))
			return errors.New("phase write failed")
		})

		require.Error(t, lp.scalePhases(3))
		assert.Equal(t, 500.0, site.pendingLoadpointDemand(lp, clck.Now()))
		assert.Equal(t, 1, lp.GetPhases())
	})

	t.Run("meterless scale down reduces claim", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		plainCharger := api.NewMockCharger(ctrl)
		phaseCharger := api.NewMockPhaseSwitcher(ctrl)
		charger := struct {
			*api.MockCharger
			*api.MockPhaseSwitcher
		}{plainCharger, phaseCharger}
		clck := clock.NewMock()
		site := &Site{log: util.NewLogger(t.Name())}
		site.initLoadpointCoordination(30 * time.Second)
		lp := newDemandTestLoadpoint(t, site, charger, clck)
		lp.phases = 3
		lp.enabled = true
		lp.status = api.StatusC
		lp.offeredCurrent = minA

		now := clck.Now()
		_, err := site.prepareLoadpointDemand(lp, 4000, 0, now, now.Add(time.Minute))
		require.NoError(t, err)
		site.commitLoadpointDemand(lp)

		expected := 4000 - currentToPower(minA, 2)
		phaseCharger.EXPECT().Phases1p3p(1).DoAndReturn(func(int) error {
			assert.Equal(t, expected, site.pendingLoadpointDemand(lp, clck.Now()))
			return nil
		})

		require.NoError(t, lp.scalePhases(1))
		assert.Equal(t, expected, site.pendingLoadpointDemand(lp, clck.Now()))
	})

	t.Run("metered scale down releases met claim", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		plainCharger := api.NewMockCharger(ctrl)
		phaseCharger := api.NewMockPhaseSwitcher(ctrl)
		charger := struct {
			*api.MockCharger
			*api.MockPhaseSwitcher
		}{plainCharger, phaseCharger}
		clck := clock.NewMock()
		site := &Site{log: util.NewLogger(t.Name())}
		site.initLoadpointCoordination(30 * time.Second)
		lp := newDemandTestLoadpoint(t, site, charger, clck)
		lp.phases = 3
		lp.enabled = true
		lp.status = api.StatusC
		lp.offeredCurrent = minA
		lp.chargeMeter = &demandTestMeter{}
		lp.demandPower = currentToPower(minA, 3)
		lp.demandValid = true

		now := clck.Now()
		_, err := site.prepareLoadpointDemand(lp, currentToPower(minA, 3), currentToPower(minA, 1), now, now.Add(time.Minute))
		require.NoError(t, err)
		site.commitLoadpointDemand(lp)

		phaseCharger.EXPECT().Phases1p3p(1).DoAndReturn(func(int) error {
			assert.Equal(t, currentToPower(minA, 2), site.pendingLoadpointDemand(lp, clck.Now()))
			return nil
		})

		require.NoError(t, lp.scalePhases(1))
		assert.Zero(t, site.pendingLoadpointDemand(lp, clck.Now()))
	})
}

func TestBatteryPowerLoadpointDemandFeedForward(t *testing.T) {
	Voltage = 230

	t.Run("retreat and block increase", func(t *testing.T) {
		f := newRegulatorTestFixture(t, -5000, -4200, 0)
		f.regulator.phase = batteryPowerCharging
		f.regulator.appliedCommand = -4200
		f.regulator.initialized = true
		f.controller.reset()

		require.NoError(t, f.regulator.setLoadpointDemand(2760, f.clock.Now().Add(time.Minute)))
		assert.Equal(t, []float64{-1440}, f.controller.values())

		f.regulator.pendingCommand = nil
		f.regulator.lastBatterySample = batteryPowerSample{Value: -1440}
		_, ok := f.regulator.increasedCommandLocked(batteryPowerCharging, -5000, -5000, false)
		assert.False(t, ok)
	})

	t.Run("timeout releases increase gate", func(t *testing.T) {
		f := newRegulatorTestFixture(t, -5000, 0, 0)
		require.NoError(t, f.regulator.setLoadpointDemand(1380, f.clock.Now().Add(batteryPowerLoadpointDemandTimeout)))
		f.clock.Add(batteryPowerLoadpointDemandTimeout + time.Second)

		_, ok := f.regulator.increasedCommandLocked(batteryPowerCharging, -5000, -5000, false)
		assert.True(t, ok)
		assert.Zero(t, f.regulator.loadpointDemand)
	})

	t.Run("partial acknowledgement then demand growth", func(t *testing.T) {
		f := newRegulatorTestFixture(t, -5000, 0, 0)
		f.regulator.phase = batteryPowerCharging
		f.regulator.appliedCommand = -5000
		f.regulator.initialized = true
		f.controller.reset()
		until := f.clock.Now().Add(time.Minute)

		require.NoError(t, f.regulator.setLoadpointDemand(1380, until))
		assert.Equal(t, []float64{-3620}, f.controller.values())

		require.NoError(t, f.regulator.setLoadpointDemand(690, until))
		assert.Equal(t, []float64{-3620}, f.controller.values(), "demand reduction does not increase charging")

		require.NoError(t, f.regulator.setLoadpointDemand(920, until))
		assert.Equal(t, []float64{-3620, -3390}, f.controller.values())
	})

	t.Run("aggregate demand changes retreat only on growth", func(t *testing.T) {
		f := newRegulatorTestFixture(t, -5000, 0, 0)
		f.regulator.phase = batteryPowerCharging
		f.regulator.appliedCommand = -5000
		f.regulator.initialized = true
		f.controller.reset()
		until := f.clock.Now().Add(time.Minute)

		for _, demand := range []float64{1000, 2000, 1500, 1800, 900, 1300} {
			require.NoError(t, f.regulator.setLoadpointDemand(demand, until))
		}
		assert.Equal(t, []float64{-4000, -3000, -2700, -2300}, f.controller.values())
	})

	t.Run("failed retreat rejects loadpoint command", func(t *testing.T) {
		f := newRegulatorTestFixture(t, -5000, -3000, 0)
		policy := f.regulator.policy
		f.regulator.phase = batteryPowerCharging
		f.regulator.appliedCommand = -3000
		f.regulator.initialized = true
		f.controller.reset()
		f.controller.fail(errors.New("battery write failed"))

		ctrl := gomock.NewController(t)
		charger := api.NewMockCharger(ctrl)
		site := &Site{
			log:                   util.NewLogger(t.Name()),
			batteryPowerRegulator: f.regulator,
		}
		site.initLoadpointCoordination(30 * time.Second)
		lp := newDemandTestLoadpoint(t, site, charger, f.clock)

		require.Error(t, lp.setLimit(minA))
		assert.Zero(t, site.pendingLoadpointDemand(lp, f.clock.Now()))
		assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
		assert.Equal(t, []float64{-1620, 0}, f.controller.values())

		f.controller.reset()
		charger.EXPECT().MaxCurrent(int64(minA)).Return(nil)
		charger.EXPECT().Enable(true).Return(nil)
		require.NoError(t, lp.setLimit(minA))
		assert.Empty(t, f.controller.values(), "fault-stopping retry must not write battery")
		assert.Equal(t, currentToPower(minA, 1), site.pendingLoadpointDemand(lp, f.clock.Now()))

		require.NoError(t, f.regulator.release())
		require.NoError(t, f.regulator.setPolicy(policy))
		assert.Equal(t, []float64{0}, f.controller.values(), "only reacquisition may write")
		_, ok := f.regulator.increasedCommandLocked(batteryPowerCharging, -5000, -5000, false)
		assert.False(t, ok, "recovery preserves unexpired demand gate")
	})

	t.Run("released records demand without writing", func(t *testing.T) {
		f := newRegulatorTestFixture(t, -5000, 0, 0)
		policy := f.regulator.policy
		require.NoError(t, f.regulator.release())
		f.controller.reset()

		require.NoError(t, f.regulator.setLoadpointDemand(1380, f.clock.Now().Add(time.Minute)))
		assert.Equal(t, 1380.0, f.regulator.loadpointDemand)
		assert.Empty(t, f.controller.values())

		require.NoError(t, f.regulator.setPolicy(policy))
		assert.Equal(t, []float64{0}, f.controller.values())
		_, ok := f.regulator.increasedCommandLocked(batteryPowerCharging, -5000, -5000, false)
		assert.False(t, ok)
	})

	t.Run("release and reacquire preserve gate", func(t *testing.T) {
		f := newRegulatorTestFixture(t, -5000, 0, 0)
		policy := f.regulator.policy
		f.regulator.phase = batteryPowerCharging
		f.regulator.appliedCommand = -5000
		f.regulator.initialized = true
		f.controller.reset()
		require.NoError(t, f.regulator.setLoadpointDemand(1380, f.clock.Now().Add(time.Minute)))
		require.NoError(t, f.regulator.release())
		require.Equal(t, 1380.0, f.regulator.loadpointDemand)

		require.NoError(t, f.regulator.setPolicy(policy))
		_, ok := f.regulator.increasedCommandLocked(batteryPowerCharging, -5000, -5000, false)
		assert.False(t, ok)

		require.NoError(t, f.regulator.setLoadpointDemand(0, time.Time{}))
		f.controller.reset()
		f.regulator.mu.Lock()
		err := f.regulator.applyCommandLocked(-2000, false, "test resumed charging")
		f.regulator.mu.Unlock()
		require.NoError(t, err)

		require.NoError(t, f.regulator.setLoadpointDemand(460, f.clock.Now().Add(time.Minute)))
		assert.Equal(t, []float64{-2000, -1540}, f.controller.values())
	})
}

func TestLoadpointDemandReevaluation(t *testing.T) {
	Voltage = 230
	clck := clock.NewMock()
	high := newPVLoadpoint(1, api.ModePV, api.StatusB, false, time.Time{})
	high.clock = clck
	low := newPVLoadpoint(0, api.ModePV, api.StatusC, true, time.Time{})
	low.clock = clck
	site := &Site{log: util.NewLogger(t.Name()), loadpoints: []*Loadpoint{high, low}}
	site.initLoadpointCoordination(30 * time.Second)

	now := clck.Now()
	_, err := site.prepareLoadpointDemand(high, 2760, 0, now, now.Add(time.Minute))
	require.NoError(t, err)
	site.commitLoadpointDemand(high)
	assert.Nil(t, site.nextLoadpointReevaluation(), "claim preparation must not schedule dependents")
	assert.Equal(t, 2760.0, site.reservedPVPower(low))

	require.NoError(t, site.observeLoadpointDemand(high, 1000, true, now.Add(time.Second)))
	assert.Same(t, low, site.nextLoadpointReevaluation(), "material partial acknowledgement")
	assert.Equal(t, 1760.0, site.pendingLoadpointDemand(high, now.Add(time.Second)))
	assert.Equal(t, 1760.0, site.reservedPVPower(low))

	require.NoError(t, site.observeLoadpointDemand(high, 2760, true, now.Add(2*time.Second)))
	assert.Same(t, low, site.nextLoadpointReevaluation(), "claim release")
	assert.Zero(t, site.pendingLoadpointDemand(high, now.Add(2*time.Second)))
}

func TestLoadpointDemandReevaluationAccumulatesSmallReductions(t *testing.T) {
	Voltage = 230
	clck := clock.NewMock()
	high := newPVLoadpoint(1, api.ModePV, api.StatusB, false, time.Time{})
	high.clock = clck
	low := newPVLoadpoint(0, api.ModePV, api.StatusC, true, time.Time{})
	low.clock = clck
	site := &Site{log: util.NewLogger(t.Name()), loadpoints: []*Loadpoint{high, low}}
	site.initLoadpointCoordination(30 * time.Second)

	now := clck.Now()
	_, err := site.prepareLoadpointDemand(high, 1380, 0, now, now.Add(time.Minute))
	require.NoError(t, err)
	site.commitLoadpointDemand(high)

	for i, observed := range []float64{100, 200} {
		require.NoError(t, site.observeLoadpointDemand(high, observed, true, now.Add(time.Duration(i+1)*time.Second)))
		assert.Nil(t, site.nextLoadpointReevaluation())
	}
	require.NoError(t, site.observeLoadpointDemand(high, 300, true, now.Add(3*time.Second)))
	assert.Same(t, low, site.nextLoadpointReevaluation())
	assert.Equal(t, 1080.0, site.reservedPVPower(low))
}

func TestLoadpointDemandPreparationDoesNotAmplifyUpdates(t *testing.T) {
	Voltage = 230
	ctrl := gomock.NewController(t)
	charger := api.NewMockCharger(ctrl)
	clck := clock.NewMock()
	site := &Site{log: util.NewLogger(t.Name())}
	site.initLoadpointCoordination(30 * time.Second)
	high := newDemandTestLoadpoint(t, site, charger, clck)
	high.priority = 1
	low := newPVLoadpoint(0, api.ModePV, api.StatusC, true, time.Time{})
	site.loadpoints = []*Loadpoint{high, low}

	charger.EXPECT().MaxCurrent(int64(minA)).Return(nil)
	charger.EXPECT().Enable(true).Return(nil)
	require.NoError(t, high.setLimit(minA))
	assert.Nil(t, site.nextLoadpointReevaluation())

	charger.EXPECT().MaxCurrent(int64(minA + 1)).Return(nil)
	require.NoError(t, high.setLimit(minA+1))
	assert.Nil(t, site.nextLoadpointReevaluation())
}

func TestLoadpointDemandCoordinationConcurrent(t *testing.T) {
	Voltage = 230
	clck := clock.NewMock()
	high := newPVLoadpoint(1, api.ModePV, api.StatusB, false, time.Time{})
	high.clock = clck
	low := newPVLoadpoint(0, api.ModePV, api.StatusC, true, time.Time{})
	low.clock = clck
	site := &Site{loadpoints: []*Loadpoint{high, low}}
	site.initLoadpointCoordination(30 * time.Second)

	now := clck.Now()
	_, err := site.prepareLoadpointDemand(high, 2760, 0, now, now.Add(time.Minute))
	require.NoError(t, err)
	site.commitLoadpointDemand(high)

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			_ = site.pendingLoadpointDemand(high, clck.Now())
		})
		wg.Go(func() {
			_ = site.observeLoadpointDemand(high, 0, false, clck.Now())
		})
	}
	wg.Wait()

	assert.Equal(t, 2760.0, site.pendingLoadpointDemand(high, clck.Now()))
}

func TestLoadpointDemandExpiryReevaluation(t *testing.T) {
	clck := clock.NewMock()
	high := newPVLoadpoint(1, api.ModePV, api.StatusB, false, time.Time{})
	high.clock = clck
	low := newPVLoadpoint(0, api.ModePV, api.StatusC, true, time.Time{})
	low.clock = clck
	site := &Site{loadpoints: []*Loadpoint{high, low}}
	site.initLoadpointCoordination(30 * time.Second)

	now := clck.Now()
	deadline := now.Add(time.Minute)
	_, err := site.prepareLoadpointDemand(high, 2760, 0, now, deadline)
	require.NoError(t, err)
	site.commitLoadpointDemand(high)
	assert.Nil(t, site.nextLoadpointReevaluation(), "claim acquisition")

	clck.Set(deadline)
	require.NoError(t, site.processCoordinationDeadlines(deadline))
	assert.Same(t, low, site.nextLoadpointReevaluation(), "claim expiry")
	assert.Zero(t, site.pendingLoadpointDemand(high, deadline))
}

func TestLoadpointCoordinationLazyInitialization(t *testing.T) {
	high := newPVLoadpoint(1, api.ModePV, api.StatusB, false, time.Time{})
	low := newPVLoadpoint(0, api.ModePV, api.StatusC, true, time.Time{})
	site := &Site{loadpoints: []*Loadpoint{high, low}}

	site.schedulePriorityDependents(high)
	assert.Same(t, low, site.nextLoadpointReevaluation())

	site.updatePVPriorityState(high)
	assert.Contains(t, site.pvPriorityStates, high)
}

var _ loadpoint.API = (*Loadpoint)(nil)
