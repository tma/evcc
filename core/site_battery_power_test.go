package core

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/types"
	"github.com/evcc-io/evcc/tariff"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testBatteryPowerLimiter struct {
	charge, discharge float64
}

func (l testBatteryPowerLimiter) GetPowerLimits() (float64, float64) {
	return l.charge, l.discharge
}

type testBatterySocLimiter struct {
	min, max float64
}

func (l testBatterySocLimiter) GetSocLimits() (float64, float64) {
	return l.min, l.max
}

type regulatorTestMeter struct {
	mu    sync.Mutex
	power float64
	err   error
	reads int
}

type blockingRegulatorTestMeter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type delayedRegulatorTestMeter struct {
	clock *clock.Mock
	delay time.Duration
	power float64
	err   error
}

type recordingRegulatorTestMeter struct {
	mu    *sync.Mutex
	order *[]string
	name  string
	power float64
}

type notifyingRegulatorTestClock struct {
	clock.Clock
	timerCreated chan struct{}
	once         sync.Once
}

func (m *blockingRegulatorTestMeter) CurrentPower() (float64, error) {
	m.once.Do(func() { close(m.started) })
	<-m.release
	return 0, nil
}

func (m *delayedRegulatorTestMeter) CurrentPower() (float64, error) {
	m.clock.Add(m.delay)
	return m.power, m.err
}

func (m *recordingRegulatorTestMeter) CurrentPower() (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	*m.order = append(*m.order, m.name)
	return m.power, nil
}

func (c *notifyingRegulatorTestClock) Timer(d time.Duration) *clock.Timer {
	timer := c.Clock.Timer(d)
	c.once.Do(func() { close(c.timerCreated) })
	return timer
}

func (m *regulatorTestMeter) CurrentPower() (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.reads++
	return m.power, m.err
}

func (m *regulatorTestMeter) set(power float64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.power = power
	m.err = err
}

func (m *regulatorTestMeter) readCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.reads
}

type regulatorTestController struct {
	mu       sync.Mutex
	commands []float64
	failNext error
}

func (c *regulatorTestController) SetBatteryPower(power float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.commands = append(c.commands, power)
	if c.failNext != nil {
		err := c.failNext
		c.failNext = nil
		return err
	}
	return nil
}

func (c *regulatorTestController) values() []float64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]float64(nil), c.commands...)
}

func (c *regulatorTestController) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.failNext = err
}

func (c *regulatorTestController) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.commands = nil
}

type regulatorTestFixture struct {
	regulator  *batteryPowerRegulator
	clock      *clock.Mock
	grid       *regulatorTestMeter
	battery    *regulatorTestMeter
	controller *regulatorTestController
}

func newRegulatorTestFixture(t *testing.T, gridPower, batteryPower, residualPower float64) *regulatorTestFixture {
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

	devices := []config.Device[api.Meter]{
		config.NewStaticDevice(config.Named{Name: "battery"}, battery),
	}
	regulator := newBatteryPowerRegulator(util.NewLogger(t.Name()), grid, devices)
	require.NotNil(t, regulator)

	clck := clock.NewMock()
	regulator.clock = clck
	require.NoError(t, regulator.setPolicy(batteryPowerControlPolicy{
		valid:            true,
		active:           true,
		chargeAllowed:    true,
		dischargeAllowed: true,
		residualPower:    residualPower,
		chargeLimit:      5000,
		dischargeLimit:   5000,
	}))
	require.Equal(t, []float64{0}, controller.values(), "activation must stop unknown prior control")
	controller.reset()
	clck.Add(batteryPowerControlInterval)

	return &regulatorTestFixture{
		regulator:  regulator,
		clock:      clck,
		grid:       grid,
		battery:    batteryMeter,
		controller: controller,
	}
}

func (f *regulatorTestFixture) step(d time.Duration) {
	if d > 0 {
		f.clock.Add(d)
	}
	f.regulator.tick()
}

func (f *regulatorTestFixture) timeoutPendingCommand() {
	f.waitBeforePendingCommandTimeout()
	f.step(batteryPowerControlInterval)
}

func (f *regulatorTestFixture) waitBeforePendingCommandTimeout() {
	steps := int(batteryPowerMaxSettleTime / batteryPowerControlInterval)
	for range steps - 1 {
		f.step(batteryPowerControlInterval)
	}
}

func TestBatteryPowerRegulatorDirectionalTargets(t *testing.T) {
	t.Run("charge preserves residual power", func(t *testing.T) {
		f := newRegulatorTestFixture(t, -3100, 0, 100)

		f.step(0)

		assert.Equal(t, []float64{-1500}, f.controller.values())
		assert.Equal(t, batteryPowerCharging, f.regulator.phase)
	})

	t.Run("discharge prefers small export", func(t *testing.T) {
		f := newRegulatorTestFixture(t, 300, 0, 100)

		f.step(0)

		assert.Equal(t, []float64{214}, f.controller.values())
		assert.Equal(t, batteryPowerDischarging, f.regulator.phase)
	})

	t.Run("negative residual does not start charge on import", func(t *testing.T) {
		f := newRegulatorTestFixture(t, 50, 0, -100)

		f.step(0)

		assert.Empty(t, f.controller.values())
	})
}

func TestBatteryPowerRegulatorConvergesOnLoadStep(t *testing.T) {
	f := newRegulatorTestFixture(t, 2000, 0, 100)

	f.step(0)
	require.Equal(t, []float64{1353}, f.controller.values())

	for _, tc := range []struct {
		battery float64
		grid    float64
		command float64
	}{
		{1353, 647, 1800},
		{1800, 200, 1947},
		{1947, 53, 1996},
		{1996, 4, 1996},
	} {
		f.battery.set(tc.battery, nil)
		f.grid.set(tc.grid, nil)
		f.step(5 * time.Second)
		commands := f.controller.values()
		assert.Equal(t, tc.command, commands[len(commands)-1])
	}
}

func TestBatteryPowerRegulatorAcquiresThroughObservedNeutral(t *testing.T) {
	f := newRegulatorTestFixture(t, 3000, -2000, 100)

	f.step(0)
	assert.Empty(t, f.controller.values())

	f.battery.set(0, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{1500}, f.controller.values())
}

func TestBatteryPowerRegulatorAcknowledgesBeforeIncrease(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)

	f.step(0)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{-1500}, f.controller.values())

	f.battery.set(-500, nil)
	f.step(5 * time.Second)

	assert.Equal(t, []float64{-1500, -3000}, f.controller.values())
}

func TestBatteryPowerIncreaseDemand(t *testing.T) {
	for _, tc := range []struct {
		name      string
		direction batteryPowerPhase
		rawError  float64
		expected  bool
	}{
		{"charge beyond deadband", batteryPowerCharging, -51, true},
		{"charge at deadband", batteryPowerCharging, -50, false},
		{"discharge beyond deadband", batteryPowerDischarging, 51, true},
		{"discharge at deadband", batteryPowerDischarging, 50, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, batteryPowerIncreaseDemand(tc.direction, tc.rawError))
		})
	}
}

func TestBatteryPowerRegulatorRollsBackUndemandedIncreaseAtTimeout(t *testing.T) {
	for _, tc := range []struct {
		name         string
		initialGrid  float64
		boundaryGrid float64
		firstCommand float64
	}{
		{"charging", -3100, -150, -1500},
		{"discharging", 3000, 30, 1500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRegulatorTestFixture(t, tc.initialGrid, 0, 100)
			f.step(0)

			f.grid.set(tc.boundaryGrid, nil)
			f.waitBeforePendingCommandTimeout()
			assert.Equal(t, []float64{tc.firstCommand}, f.controller.values(), "must preserve the acknowledgement window")
			require.NotNil(t, f.regulator.pendingCommand)

			f.step(batteryPowerControlInterval)

			assert.Equal(t, []float64{tc.firstCommand, 0}, f.controller.values())
			assert.Equal(t, batteryPowerNeutral, f.regulator.phase)
			assert.Nil(t, f.regulator.pendingCommand)
			assert.True(t, f.regulator.neutralRequired)
			assert.True(t, f.regulator.chargeBlockedUntil.IsZero())
			assert.True(t, f.regulator.dischargeBlockedUntil.IsZero())
		})
	}
}

func TestBatteryPowerRegulatorRollbackWaitsForReductionAcknowledgement(t *testing.T) {
	f := newRegulatorTestFixture(t, 3000, 0, 100)
	f.step(0)

	f.grid.set(-20, nil)
	f.battery.set(1500, nil)
	f.step(batteryPowerControlInterval)
	require.Nil(t, f.regulator.pendingCommand)

	cooldownHistory := f.clock.Now().Add(-time.Second)
	f.regulator.dischargeBlockedUntil = cooldownHistory
	f.grid.set(3000, nil)
	f.step(batteryPowerControlInterval)
	require.Equal(t, []float64{1500, 3000}, f.controller.values())

	f.grid.set(-20, nil)
	f.waitBeforePendingCommandTimeout()
	assert.Equal(t, []float64{1500, 3000}, f.controller.values(), "must preserve the acknowledgement window")

	f.step(batteryPowerControlInterval)
	require.Equal(t, []float64{1500, 3000, 1500}, f.controller.values())
	require.NotNil(t, f.regulator.pendingCommand)
	assert.Equal(t, 3000.0, f.regulator.pendingCommand.PreviousCommand)
	assert.Equal(t, 1500.0, f.regulator.pendingCommand.Command)
	assert.Equal(t, cooldownHistory, f.regulator.dischargeBlockedUntil)
	assert.True(t, f.regulator.chargeBlockedUntil.IsZero())

	f.grid.set(3000, nil)
	f.battery.set(3000, nil)
	f.step(batteryPowerControlInterval)
	assert.Equal(t, []float64{1500, 3000, 1500}, f.controller.values(), "rollback must settle before another increase")
	assert.Equal(t, cooldownHistory, f.regulator.dischargeBlockedUntil)

	f.battery.set(1500, nil)
	f.step(batteryPowerControlInterval)
	assert.Equal(t, []float64{1500, 3000, 1500, 3000}, f.controller.values())
	assert.Equal(t, cooldownHistory, f.regulator.dischargeBlockedUntil, "rollback acknowledgement must not clear cooldown history")
}

func TestBatteryPowerRegulatorRollbackToZeroRequiresNeutral(t *testing.T) {
	f := newRegulatorTestFixture(t, 3000, 0, 100)
	f.step(0)

	f.grid.set(-20, nil)
	f.waitBeforePendingCommandTimeout()
	assert.Equal(t, []float64{1500}, f.controller.values(), "must preserve the acknowledgement window")

	f.step(batteryPowerControlInterval)
	require.Equal(t, []float64{1500, 0}, f.controller.values())
	require.True(t, f.regulator.neutralRequired)

	f.grid.set(3000, nil)
	f.battery.set(500, nil)
	f.step(batteryPowerControlInterval)
	assert.Equal(t, []float64{1500, 0}, f.controller.values())

	f.battery.set(0, nil)
	f.step(batteryPowerControlInterval)
	assert.Equal(t, []float64{1500, 0, 1500}, f.controller.values())
}

func TestBatteryPowerRegulatorRollbackWriteFailureStopsAndFaults(t *testing.T) {
	f := newRegulatorTestFixture(t, 3000, 0, 100)
	f.step(0)

	f.grid.set(-20, nil)
	f.battery.set(1500, nil)
	f.step(batteryPowerControlInterval)
	f.grid.set(3000, nil)
	f.step(batteryPowerControlInterval)
	require.Equal(t, []float64{1500, 3000}, f.controller.values())

	rollbackErr := errors.New("rollback failed")
	f.controller.fail(rollbackErr)
	f.grid.set(-20, nil)
	f.waitBeforePendingCommandTimeout()
	f.step(batteryPowerControlInterval)

	assert.Equal(t, []float64{1500, 3000, 1500, 0}, f.controller.values())
	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, 0.0, f.regulator.appliedCommand)
	assert.Nil(t, f.regulator.pendingCommand)
}

func TestBatteryPowerRegulatorSafetyRetreatPrecedesTimeoutRollback(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)
	f.waitBeforePendingCommandTimeout()

	f.grid.set(1000, nil)
	f.step(batteryPowerControlInterval)

	assert.Equal(t, []float64{-1500, -400}, f.controller.values())
	assert.Equal(t, batteryPowerCharging, f.regulator.phase)
	require.NotNil(t, f.regulator.pendingCommand)
	assert.Equal(t, -400.0, f.regulator.pendingCommand.Command)
	assert.True(t, f.regulator.chargeBlockedUntil.IsZero())
}

func TestBatteryPowerRegulatorForceChargeTimeoutDoesNotRollback(t *testing.T) {
	f := newRegulatorTestFixture(t, 0, 0, 100)
	f.regulator.policyMaxAge = time.Hour
	policy := f.regulator.policy
	policy.forceCharge = true
	require.NoError(t, f.regulator.setPolicy(policy))

	f.step(0)
	f.timeoutPendingCommand()

	assert.Equal(t, []float64{-1500, 0}, f.controller.values())
	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, f.clock.Now().Add(batteryPowerFirstCooldown), f.regulator.chargeBlockedUntil)
}

func TestBatteryPowerRegulatorSafetyRetreatWaitsForFeedback(t *testing.T) {
	t.Run("charging", func(t *testing.T) {
		f := newRegulatorTestFixture(t, -3100, 0, 100)
		f.step(0)

		f.grid.set(1000, nil)
		reads := f.battery.readCount()
		f.step(5 * time.Second)

		assert.Equal(t, []float64{-1500, -400}, f.controller.values())
		require.NotNil(t, f.regulator.pendingCommand)
		assert.Equal(t, reads+1, f.battery.readCount(), "every cycle must use fresh battery feedback")

		f.grid.set(-1000, nil)
		f.battery.set(-1000, nil)
		f.step(5 * time.Second)
		assert.Equal(t, []float64{-1500, -400}, f.controller.values(), "must not increase before the retreat settles")

		f.battery.set(-600, nil)
		f.step(5 * time.Second)
		assert.Equal(t, []float64{-1500, -400, -1003}, f.controller.values())
	})

	t.Run("discharging", func(t *testing.T) {
		f := newRegulatorTestFixture(t, 3000, 0, 100)
		f.step(0)

		f.grid.set(-1000, nil)
		f.step(5 * time.Second)
		assert.Equal(t, []float64{1500, 520}, f.controller.values())
		require.NotNil(t, f.regulator.pendingCommand)

		f.grid.set(1000, nil)
		f.battery.set(900, nil)
		f.step(5 * time.Second)
		assert.Equal(t, []float64{1500, 520}, f.controller.values(), "must not increase before the retreat settles")

		f.battery.set(700, nil)
		f.step(5 * time.Second)
		assert.Equal(t, []float64{1500, 520, 1203}, f.controller.values())
	})
}

func TestBatteryPowerRegulatorAllowsFurtherSafetyRetreat(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	f.grid.set(500, nil)
	f.step(5 * time.Second)
	require.Equal(t, []float64{-1500, -900}, f.controller.values())

	f.grid.set(200, nil)
	f.battery.set(-1500, nil)
	f.step(5 * time.Second)

	assert.Equal(t, []float64{-1500, -900, -600}, f.controller.values())
}

func TestBatteryPowerRegulatorSafetyRetreatTimeout(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	f.grid.set(1000, nil)
	f.step(5 * time.Second)
	require.Equal(t, []float64{-1500, -400}, f.controller.values())

	f.grid.set(-100, nil)
	f.battery.set(-1500, nil)
	var logs bytes.Buffer
	f.regulator.log.SetLogOutput(&logs)
	cooldownHistory := f.clock.Now().Add(-time.Second)
	f.regulator.chargeBlockedUntil = cooldownHistory
	for range 6 {
		f.step(5 * time.Second)
	}

	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, []float64{-1500, -400, 0}, f.controller.values())
	assert.Equal(t, cooldownHistory, f.regulator.chargeBlockedUntil)
	assert.True(t, f.regulator.dischargeBlockedUntil.IsZero())
	assert.Contains(t, logs.String(), "command acknowledgement timed out: direction=charging command=-400W previous=-1500W")
	assert.NotContains(t, logs.String(), "repeated command acknowledgement timeout")
}

func TestBatteryPowerRegulatorRetreatSnapsSmallCommandToZero(t *testing.T) {
	f := newRegulatorTestFixture(t, -500, 0, 100)
	f.step(0)
	require.Equal(t, []float64{-268}, f.controller.values())

	f.grid.set(150, nil)
	f.battery.set(-100, nil)
	f.step(5 * time.Second)

	assert.Equal(t, []float64{-268, 0}, f.controller.values())
	assert.Equal(t, batteryPowerNeutral, f.regulator.phase)
}

func TestBatteryPowerRegulatorIgnoresNoiseInsideDeadband(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	f.grid.set(-50, nil)
	f.battery.set(-500, nil)
	f.step(5 * time.Second)

	assert.Equal(t, []float64{-1500}, f.controller.values())
}

func TestBatteryPowerRegulatorKeepsStartupDeadband(t *testing.T) {
	f := newRegulatorTestFixture(t, 75, 0, 100)
	f.step(0)
	assert.Empty(t, f.controller.values())

	f.grid.set(101, nil)
	f.step(5 * time.Second)

	assert.Equal(t, []float64{81}, f.controller.values())
	assert.Equal(t, batteryPowerDischarging, f.regulator.phase)
}

func TestBatteryPowerRegulatorCorrectsLowPowerImport(t *testing.T) {
	f := newRegulatorTestFixture(t, 300, 0, 100)
	f.step(0)
	require.Equal(t, []float64{214}, f.controller.values())

	f.grid.set(80, nil)
	f.battery.set(160, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{214, 281}, f.controller.values())

	f.step(5 * time.Second)
	assert.Equal(t, []float64{214, 281}, f.controller.values(), "must wait for low-power feedback")

	f.grid.set(0, nil)
	f.battery.set(281, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{214, 281}, f.controller.values())
}

func TestBatteryPowerRegulatorUsesBiasedDischargeDeadband(t *testing.T) {
	f := newRegulatorTestFixture(t, 300, 0, 100)
	f.step(0)
	require.Equal(t, []float64{214}, f.controller.values())

	f.grid.set(30, nil)
	f.battery.set(160, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{214}, f.controller.values(), "30W import is inside the deadband")

	f.grid.set(31, nil)
	f.step(5 * time.Second)
	require.Equal(t, []float64{214, 248}, f.controller.values(), "import beyond the deadband must increase discharge")

	f.grid.set(-70, nil)
	f.battery.set(248, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{214, 248}, f.controller.values(), "70W export is inside the deadband")

	f.grid.set(-71, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{214, 248, 197}, f.controller.values(), "export beyond the deadband must retreat")
}

func TestBatteryPowerRegulatorAcknowledgesSmallCorrectionMovement(t *testing.T) {
	f := newRegulatorTestFixture(t, 300, 0, 100)
	f.step(0)
	require.Equal(t, []float64{214}, f.controller.values())

	f.grid.set(30.2, nil)
	f.battery.set(130, nil)
	f.step(5 * time.Second)
	require.Equal(t, []float64{214, 248}, f.controller.values())

	f.step(5 * time.Second)
	assert.Equal(t, []float64{214, 248}, f.controller.values(), "unchanged feedback must not acknowledge")

	f.battery.set(141, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{214, 248, 282}, f.controller.values(), "directional movement must acknowledge")
}

func TestBatteryPowerRegulatorWaitsForLowPowerRetreat(t *testing.T) {
	f := newRegulatorTestFixture(t, 300, 0, 100)
	f.step(0)
	require.Equal(t, []float64{214}, f.controller.values())

	f.grid.set(-80, nil)
	f.battery.set(214, nil)
	f.step(5 * time.Second)
	require.Equal(t, []float64{214, 154}, f.controller.values())

	f.grid.set(80, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{214, 154}, f.controller.values(), "must wait for the retreat to settle")

	f.battery.set(154, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{214, 154, 221}, f.controller.values())
}

func TestBatteryPowerRegulatorStaggersPeriodicReads(t *testing.T) {
	f := newRegulatorTestFixture(t, 0, 0, 100)
	initialGridReads := f.grid.readCount()
	initialBatteryReads := f.battery.readCount()
	timerCreated := make(chan struct{})
	f.regulator.clock = &notifyingRegulatorTestClock{
		Clock:        f.clock,
		timerCreated: timerCreated,
	}

	f.regulator.start()
	require.Eventually(t, func() bool {
		return f.grid.readCount() == initialGridReads+1 &&
			f.battery.readCount() == initialBatteryReads+1
	}, time.Second, time.Millisecond)
	<-timerCreated

	f.clock.Add(7 * time.Second)
	assert.Equal(t, initialGridReads+1, f.grid.readCount())
	assert.Equal(t, initialBatteryReads+1, f.battery.readCount())

	f.clock.Add(500 * time.Millisecond)
	require.Eventually(t, func() bool {
		return f.grid.readCount() == initialGridReads+2 &&
			f.battery.readCount() == initialBatteryReads+2
	}, time.Second, time.Millisecond)

	require.NoError(t, f.regulator.stop())
}

func TestBatteryPowerRegulatorStartsFromSingleSample(t *testing.T) {
	f := newRegulatorTestFixture(t, -500, 0, 100)

	f.step(0)
	assert.Equal(t, []float64{-268}, f.controller.values())
}

func TestBatteryPowerRegulatorObservedZeroBeforeReversal(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	f.battery.set(-500, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{-1500, -3000}, f.controller.values())

	f.grid.set(4000, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{-1500, -3000, 0}, f.controller.values())

	f.battery.set(-500, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{-1500, -3000, 0}, f.controller.values())

	f.battery.set(0, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{-1500, -3000, 0, 1500}, f.controller.values())
}

func TestBatteryPowerRegulatorDelayedAcknowledgement(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	for range 3 {
		f.step(5 * time.Second)
	}
	assert.Equal(t, batteryPowerCharging, f.regulator.phase)
	assert.Equal(t, []float64{-1500}, f.controller.values())

	f.battery.set(-500, nil)
	f.step(5 * time.Second)

	assert.NotEqual(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, []float64{-1500, -3000}, f.controller.values())
}

func TestBatteryPowerRegulatorAcknowledgementTimeout(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.regulator.policy.soc = 99
	f.regulator.policy.minSoc = 5
	f.regulator.policy.maxSoc = 100
	f.regulator.policy.socLimitsValid = true
	f.regulator.policyMaxAge = time.Hour

	var logs bytes.Buffer
	f.regulator.log.SetLogOutput(&logs)
	f.step(0)
	f.timeoutPendingCommand()

	firstBlockedUntil := f.clock.Now().Add(batteryPowerFirstCooldown)
	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, []float64{-1500, 0}, f.controller.values())
	assert.Equal(t, firstBlockedUntil, f.regulator.chargeBlockedUntil)
	assert.Contains(t, logs.String(), "command acknowledgement timed out: direction=charging command=-1500W previous=0W battery-baseline=0W battery-final=0W grid=-3100W soc=99.0% (limits 5.0%..100.0%) elapsed=30s cooldown=1m0s next=neutral-feedback")
	assert.Contains(t, logs.String(), "charging blocked for 1m0s after command acknowledgement timeout")

	f.step(batteryPowerControlInterval)
	f.step(batteryPowerFirstCooldown - 2*batteryPowerControlInterval)
	assert.Equal(t, []float64{-1500, 0}, f.controller.values(), "cooldown must block retries before expiry")

	f.step(batteryPowerControlInterval)
	require.Equal(t, []float64{-1500, 0, -1500}, f.controller.values(), "continued demand may retry at expiry")
	f.timeoutPendingCommand()

	repeatedBlockedUntil := f.clock.Now().Add(batteryPowerRepeatedCooldown)
	assert.Equal(t, repeatedBlockedUntil, f.regulator.chargeBlockedUntil)
	assert.Equal(t, 1, strings.Count(logs.String(), "command acknowledgement timed out: direction=charging"))
	assert.Contains(t, logs.String(), "repeated command acknowledgement timeout: direction=charging")
	assert.Contains(t, logs.String(), "charging blocked for 10m0s after command acknowledgement timeout")

	f.step(batteryPowerControlInterval)
	f.step(batteryPowerRepeatedCooldown - 2*batteryPowerControlInterval)
	assert.Equal(t, []float64{-1500, 0, -1500, 0}, f.controller.values(), "repeated cooldown must block retries before expiry")

	f.step(batteryPowerControlInterval)
	require.Equal(t, []float64{-1500, 0, -1500, 0, -1500}, f.controller.values())
	f.battery.set(-500, nil)
	f.step(batteryPowerControlInterval)

	assert.True(t, f.regulator.chargeBlockedUntil.IsZero())
	assert.Contains(t, logs.String(), "charging command acknowledged; cooldown history cleared")
}

func TestBatteryPowerRegulatorCooldownKeepsOppositeDirectionAvailable(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.regulator.policyMaxAge = time.Hour
	f.step(0)
	f.timeoutPendingCommand()
	chargeBlockedUntil := f.regulator.chargeBlockedUntil

	f.step(batteryPowerControlInterval)
	f.grid.set(3000, nil)
	f.step(batteryPowerControlInterval)

	assert.Equal(t, []float64{-1500, 0, 1500}, f.controller.values())
	assert.Equal(t, batteryPowerDischarging, f.regulator.phase)
	assert.Equal(t, chargeBlockedUntil, f.regulator.chargeBlockedUntil)

	f.battery.set(500, nil)
	f.step(batteryPowerControlInterval)
	assert.Equal(t, chargeBlockedUntil, f.regulator.chargeBlockedUntil, "opposite-direction acknowledgement must not clear charge history")
}

func TestBatteryPowerRegulatorReductionAcknowledgementKeepsCooldownHistory(t *testing.T) {
	f := newRegulatorTestFixture(t, -100, 0, 100)
	blockedUntil := f.clock.Now().Add(-time.Second)
	f.regulator.chargeBlockedUntil = blockedUntil
	f.regulator.pendingCommand = &pendingBatteryPowerCommand{
		PreviousCommand: -1500,
		Command:         -400,
		BaselinePower:   0,
		AppliedAt:       f.clock.Now().Add(-batteryPowerControlInterval),
	}

	now := f.clock.Now()
	f.regulator.updateAcknowledgementLocked(batteryPowerSample{
		Value:      0,
		StartedAt:  now,
		FinishedAt: now,
	})

	assert.Nil(t, f.regulator.pendingCommand)
	assert.Equal(t, blockedUntil, f.regulator.chargeBlockedUntil)
}

func TestBatteryPowerRegulatorCooldownSurvivesModeHandoff(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	chargeBlockedUntil := f.clock.Now().Add(batteryPowerFirstCooldown)
	dischargeBlockedUntil := f.clock.Now().Add(batteryPowerRepeatedCooldown)
	f.regulator.chargeBlockedUntil = chargeBlockedUntil
	f.regulator.dischargeBlockedUntil = dischargeBlockedUntil

	activePolicy := f.regulator.policy
	releasedPolicy := activePolicy
	releasedPolicy.active = false
	require.NoError(t, f.regulator.setPolicy(releasedPolicy))
	require.NoError(t, f.regulator.setPolicy(activePolicy))

	assert.Equal(t, chargeBlockedUntil, f.regulator.chargeBlockedUntil)
	assert.Equal(t, dischargeBlockedUntil, f.regulator.dischargeBlockedUntil)
	f.controller.reset()
	f.step(batteryPowerControlInterval)
	assert.Empty(t, f.controller.values(), "reacquiring control must not bypass the cooldown")
}

func TestBatteryPowerRegulatorForceChargeRespectsCooldown(t *testing.T) {
	f := newRegulatorTestFixture(t, 3000, 0, 100)
	f.regulator.policyMaxAge = time.Hour
	f.regulator.chargeBlockedUntil = f.clock.Now().Add(batteryPowerFirstCooldown)
	policy := f.regulator.policy
	policy.forceCharge = true
	require.NoError(t, f.regulator.setPolicy(policy))

	f.step(0)
	assert.Empty(t, f.controller.values())

	f.step(batteryPowerFirstCooldown)
	assert.Equal(t, []float64{-1500}, f.controller.values())
}

func TestBatteryPowerRegulatorBatteryReadFailure(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	var logs bytes.Buffer
	f.regulator.log.SetLogOutput(&logs)
	f.regulator.battery.meter = &delayedRegulatorTestMeter{
		clock: f.clock,
		delay: 6 * time.Second,
		err:   errors.New("modbus exception 4"),
	}
	f.step(5 * time.Second)

	assert.Equal(t, batteryPowerCharging, f.regulator.phase)
	assert.Equal(t, []float64{-1500}, f.controller.values())
	assert.Contains(t, logs.String(), "battery power control: battery feedback unavailable: modbus exception 4; battery read duration: 6s; last valid sample age: 11s; holding -1500W")
}

func TestBatteryPowerRegulatorReadsGridAfterBattery(t *testing.T) {
	f := newRegulatorTestFixture(t, 0, 0, 100)
	var (
		mu    sync.Mutex
		order []string
	)
	f.regulator.battery.meter = &recordingRegulatorTestMeter{mu: &mu, order: &order, name: "battery"}
	f.regulator.gridMeter = &recordingRegulatorTestMeter{mu: &mu, order: &order, name: "grid"}

	f.step(0)

	assert.Equal(t, []string{"battery", "grid"}, order)
}

func TestBatteryPowerRegulatorAcceptsSlowBatteryRead(t *testing.T) {
	f := newRegulatorTestFixture(t, 300, 0, 100)
	f.step(0)
	require.Equal(t, []float64{214}, f.controller.values())

	f.regulator.battery.meter = &delayedRegulatorTestMeter{
		clock: f.clock,
		delay: 16 * time.Second,
		power: 150,
	}
	f.grid.set(80, nil)
	gridReads := f.grid.readCount()
	f.step(5 * time.Second)

	assert.Equal(t, batteryPowerDischarging, f.regulator.phase)
	assert.Equal(t, []float64{214, 281}, f.controller.values(), "fresh feedback must resume control after a slow read")
	assert.Equal(t, gridReads+1, f.grid.readCount(), "grid must be read after the slow battery response")
}

func TestBatteryPowerRegulatorRecoversFeedbackDuringGrace(t *testing.T) {
	f := newRegulatorTestFixture(t, 300, 0, 100)
	f.step(0)
	require.Equal(t, []float64{214}, f.controller.values())

	f.battery.set(0, errors.New("read failed"))
	f.grid.set(40, nil)
	f.step(5 * time.Second)
	require.Equal(t, []float64{214}, f.controller.values())

	f.battery.set(214, nil)
	f.step(5 * time.Second)

	assert.Equal(t, batteryPowerDischarging, f.regulator.phase)
	assert.Equal(t, []float64{214, 254}, f.controller.values())
}

func TestBatteryPowerRegulatorFeedbackGraceExpires(t *testing.T) {
	f := newRegulatorTestFixture(t, 300, 0, 100)
	f.step(0)
	require.Equal(t, []float64{214}, f.controller.values())

	f.battery.set(0, errors.New("read failed"))
	f.grid.set(40, nil)
	f.step(5 * time.Second)
	require.Equal(t, []float64{214}, f.controller.values())

	f.step(10 * time.Second)

	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, []float64{214, 0}, f.controller.values())
}

func TestBatteryPowerRegulatorFeedbackGraceRequiresPriorSample(t *testing.T) {
	f := newRegulatorTestFixture(t, 300, 0, 100)
	var logs bytes.Buffer
	f.regulator.log.SetLogOutput(&logs)
	f.battery.set(0, errors.New("read failed"))
	f.step(0)

	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Empty(t, f.controller.values())
	assert.Contains(t, logs.String(), "battery feedback unavailable: read failed; battery read duration: 0s; last valid sample age: unavailable")
}

func TestBatteryPowerRegulatorNeutralFeedbackFailureDoesNotRewriteZero(t *testing.T) {
	f := newRegulatorTestFixture(t, 0, 0, 100)
	f.step(0)
	require.Equal(t, batteryPowerNeutral, f.regulator.phase)
	require.Empty(t, f.controller.values())

	f.battery.set(0, errors.New("read failed"))
	f.step(5 * time.Second)
	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Empty(t, f.controller.values())

	f.step(5 * time.Second)
	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Empty(t, f.controller.values())

	f.battery.set(0, nil)
	f.step(5 * time.Second)
	assert.Equal(t, batteryPowerNeutral, f.regulator.phase)
	assert.Empty(t, f.controller.values())

	f.battery.set(0, errors.New("read failed again"))
	f.step(5 * time.Second)
	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Empty(t, f.controller.values())
}

func TestBatteryPowerRegulatorFeedbackGraceOnlyRetreats(t *testing.T) {
	f := newRegulatorTestFixture(t, 3000, 0, 100)
	f.step(0)
	require.Equal(t, []float64{1500}, f.controller.values())

	f.battery.set(0, errors.New("read failed"))
	f.grid.set(-1000, nil)
	f.step(5 * time.Second)
	require.Equal(t, []float64{1500, 520}, f.controller.values())

	f.grid.set(1000, nil)
	f.step(5 * time.Second)

	assert.Equal(t, batteryPowerDischarging, f.regulator.phase)
	assert.Equal(t, []float64{1500, 520}, f.controller.values(), "feedback grace must not increase power")
}

func TestBatteryPowerRegulatorGridReadFailureStopsImmediately(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	f.battery.set(0, errors.New("battery read failed"))
	f.grid.set(0, errors.New("read failed"))
	f.step(5 * time.Second)

	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, []float64{-1500, 0}, f.controller.values())
}

func TestBatteryPowerRegulatorSlowGridReadStopsImmediately(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	f.battery.set(-1500, nil)
	f.regulator.gridMeter = &delayedRegulatorTestMeter{
		clock: f.clock,
		delay: 5 * time.Second,
		power: -100,
	}
	f.step(5 * time.Second)

	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, []float64{-1500, 0}, f.controller.values())
}

func TestBatteryPowerRegulatorGridFailureLogsBatteryRead(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	var logs bytes.Buffer
	f.regulator.log.SetLogOutput(&logs)
	f.regulator.battery.meter = &delayedRegulatorTestMeter{
		clock: f.clock,
		delay: 6 * time.Second,
		err:   errors.New("modbus exception 4"),
	}
	f.grid.set(0, errors.New("shelly timeout"))
	f.step(5 * time.Second)

	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, []float64{-1500, 0}, f.controller.values())
	assert.Contains(t, logs.String(), "grid unavailable: shelly timeout; grid read: shelly timeout after 0s; battery read: modbus exception 4 after 6s")
}

func TestBatteryPowerRegulatorRearmLogsBatteryPower(t *testing.T) {
	f := newRegulatorTestFixture(t, 42, 250, 100)
	f.regulator.phase = batteryPowerFaultStopping
	f.regulator.appliedCommand = 0
	f.regulator.initialized = true
	f.regulator.lastWriteAt = f.clock.Now().Add(-time.Second)

	var logs bytes.Buffer
	f.regulator.log.SetLogOutput(&logs)
	f.step(0)

	assert.Equal(t, batteryPowerNeutral, f.regulator.phase)
	assert.Contains(t, logs.String(), "battery power control: rearmed at battery 250W, grid 42W")
}

func TestBatteryPowerRegulatorFailedWriteStopsAndFaults(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.controller.fail(errors.New("write failed"))

	f.step(0)

	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, 0.0, f.regulator.appliedCommand)
	assert.Equal(t, []float64{-1500, 0}, f.controller.values())
}

func TestBatteryPowerRegulatorFailedStopDoesNotRetryImmediately(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	stopErr := errors.New("stop failed")
	f.controller.fail(stopErr)

	require.ErrorIs(t, f.regulator.release(), stopErr)
	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, -1500.0, f.regulator.appliedCommand)
	assert.Equal(t, []float64{-1500, 0}, f.controller.values())
}

func TestBatteryPowerRegulatorRejectsMultipleControllers(t *testing.T) {
	grid := &regulatorTestMeter{power: -3100}
	firstMeter := &regulatorTestMeter{}
	secondMeter := &regulatorTestMeter{}
	firstController := &regulatorTestController{}
	secondController := &regulatorTestController{}
	limits := testBatteryPowerLimiter{charge: 5000, discharge: 5000}

	var first api.Meter = &struct {
		api.Meter
		api.BatteryPowerController
		api.BatteryPowerLimiter
	}{firstMeter, firstController, limits}
	var second api.Meter = &struct {
		api.Meter
		api.BatteryPowerController
		api.BatteryPowerLimiter
	}{secondMeter, secondController, limits}
	devices := []config.Device[api.Meter]{
		config.NewStaticDevice(config.Named{Name: "first"}, first),
		config.NewStaticDevice(config.Named{Name: "second"}, second),
	}
	regulator := newBatteryPowerRegulator(util.NewLogger("test"), grid, devices)

	assert.Nil(t, regulator)
	assert.Empty(t, firstController.values())
	assert.Empty(t, secondController.values())
}

func TestBatteryPowerRegulatorRefreshesUnchangedCommand(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	f.grid.set(-100, nil)
	f.battery.set(-500, nil)
	f.step(5 * time.Second)
	require.Equal(t, []float64{-1500}, f.controller.values())

	f.step(25 * time.Second)
	assert.Equal(t, []float64{-1500, -1500}, f.controller.values())
}

func TestBatteryPowerRegulatorStopsOnStalePolicy(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	f.step(61 * time.Second)

	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, []float64{-1500, 0}, f.controller.values())
}

func TestBatteryPowerRegulatorForceCharge(t *testing.T) {
	f := newRegulatorTestFixture(t, 3000, 0, 100)

	policy := f.regulator.policy
	policy.forceCharge = true
	require.NoError(t, f.regulator.setPolicy(policy))
	f.step(0)

	assert.Equal(t, []float64{-1500}, f.controller.values())
}

func TestBatteryPowerRegulatorRelease(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	require.NoError(t, f.regulator.release())

	assert.Equal(t, batteryPowerReleased, f.regulator.phase)
	assert.Equal(t, []float64{-1500, 0}, f.controller.values())

	gridReads := f.grid.readCount()
	batteryReads := f.battery.readCount()
	f.step(5 * time.Second)
	assert.Equal(t, gridReads, f.grid.readCount())
	assert.Equal(t, batteryReads, f.battery.readCount())
}

func TestBatteryPowerRegulatorStopReleasesBeforeBlockedReadReturns(t *testing.T) {
	grid := &regulatorTestMeter{power: -3100}
	blockedGrid := &blockingRegulatorTestMeter{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	batteryMeter := &regulatorTestMeter{}
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
	devices := []config.Device[api.Meter]{
		config.NewStaticDevice(config.Named{Name: "battery"}, battery),
	}
	regulator := newBatteryPowerRegulator(util.NewLogger("test"), grid, devices)
	require.NotNil(t, regulator)
	require.NoError(t, regulator.setPolicy(batteryPowerControlPolicy{
		valid:            true,
		active:           true,
		chargeAllowed:    true,
		dischargeAllowed: true,
		chargeLimit:      5000,
		dischargeLimit:   5000,
	}))
	regulator.tick()
	require.Equal(t, batteryPowerCharging, regulator.phase)
	controller.reset()
	regulator.gridMeter = blockedGrid
	regulator.start()
	<-blockedGrid.started

	stopped := make(chan error, 1)
	go func() {
		stopped <- regulator.stop()
	}()

	require.Eventually(t, func() bool {
		return len(controller.values()) == 1
	}, time.Second, time.Millisecond)
	assert.Equal(t, []float64{0}, controller.values())

	close(blockedGrid.release)
	require.NoError(t, <-stopped)
}

func TestBatteryPowerControlFallbackRetriesRelease(t *testing.T) {
	controller := &regulatorTestController{}
	controller.fail(errors.New("stop failed"))
	var battery api.Meter = &struct {
		api.Meter
		api.BatteryPowerController
	}{
		BatteryPowerController: controller,
	}
	site := &Site{
		log:           util.NewLogger("test"),
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{Name: "battery"}, battery)},
	}

	site.updateBatteryPowerControlPolicy(api.Rate{}, true)
	assert.False(t, site.batteryPowerReleased)
	site.updateBatteryPowerControlPolicy(api.Rate{}, true)
	assert.True(t, site.batteryPowerReleased)
	site.updateBatteryPowerControlPolicy(api.Rate{}, true)

	assert.Equal(t, []float64{0, 0}, controller.values())
}

func TestBatteryPowerControlPolicy(t *testing.T) {
	grid := &regulatorTestMeter{}
	batteryMeter := &regulatorTestMeter{}
	controller := &regulatorTestController{}

	var battery api.Meter = &struct {
		api.Meter
		api.BatteryPowerController
		api.BatteryPowerLimiter
		api.BatterySocLimiter
	}{
		Meter:                  batteryMeter,
		BatteryPowerController: controller,
		BatteryPowerLimiter:    testBatteryPowerLimiter{charge: 5000, discharge: 5000},
		BatterySocLimiter:      testBatterySocLimiter{min: 20, max: 95},
	}

	devices := []config.Device[api.Meter]{
		config.NewStaticDevice(config.Named{Name: "battery"}, battery),
	}
	site := &Site{
		log:                   util.NewLogger("test"),
		gridMeter:             config.NewStaticDevice[api.Meter](config.Named{Name: "grid"}, grid),
		batteryMeters:         devices,
		batteryMode:           api.BatteryNormal,
		tariffs:               &tariff.Tariffs{},
		ResidualPower:         100,
		batteryPowerRegulator: newBatteryPowerRegulator(util.NewLogger("test"), grid, devices),
	}

	soc := 50.0
	site.battery.Devices = []types.Measurement{{Soc: &soc}}

	policy := site.batteryPowerControlPolicy(api.Rate{})
	require.True(t, policy.valid)
	require.True(t, policy.active)
	assert.Equal(t, 100.0, policy.residualPower)
	assert.Equal(t, 50.0, policy.soc)
	assert.Equal(t, 20.0, policy.minSoc)
	assert.Equal(t, 95.0, policy.maxSoc)
	assert.True(t, policy.socLimitsValid)
	assert.True(t, policy.chargeAllowed)
	assert.True(t, policy.dischargeAllowed)

	soc = 95
	policy = site.batteryPowerControlPolicy(api.Rate{})
	assert.False(t, policy.chargeAllowed)
	assert.True(t, policy.dischargeAllowed)

	soc = 20
	policy = site.batteryPowerControlPolicy(api.Rate{})
	assert.True(t, policy.chargeAllowed)
	assert.False(t, policy.dischargeAllowed)
}

func TestBatteryPowerControlPolicyRequiresSocLimits(t *testing.T) {
	grid := &regulatorTestMeter{}
	batteryMeter := &regulatorTestMeter{}
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

	devices := []config.Device[api.Meter]{
		config.NewStaticDevice(config.Named{Name: "battery"}, battery),
	}
	site := &Site{
		log:                   util.NewLogger("test"),
		gridMeter:             config.NewStaticDevice[api.Meter](config.Named{Name: "grid"}, grid),
		batteryMeters:         devices,
		batteryMode:           api.BatteryNormal,
		tariffs:               &tariff.Tariffs{},
		ResidualPower:         100,
		batteryPowerRegulator: newBatteryPowerRegulator(util.NewLogger("test"), grid, devices),
	}

	policy := site.batteryPowerControlPolicy(api.Rate{})

	assert.False(t, policy.active)
	assert.False(t, policy.chargeAllowed)
	assert.False(t, policy.dischargeAllowed)
}
