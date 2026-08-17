package core

import (
	"bytes"
	"errors"
	"fmt"
	"math"
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
	started  chan struct{}
	release  chan struct{}
	done     chan struct{}
	power    float64
	once     sync.Once
	doneOnce sync.Once
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
	m.once.Do(func() {
		if m.started != nil {
			close(m.started)
		}
	})
	if m.release != nil {
		<-m.release
	}
	m.doneOnce.Do(func() {
		if m.done != nil {
			close(m.done)
		}
	})
	return m.power, nil
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
	mu        sync.Mutex
	commands  []float64
	failNext  error
	failAll   error
	started   chan struct{}
	blockNext chan struct{}
}

func (c *regulatorTestController) SetBatteryPower(power float64) error {
	c.mu.Lock()
	c.commands = append(c.commands, power)
	var err error
	if c.failAll != nil {
		err = c.failAll
	} else if c.failNext != nil {
		err = c.failNext
		c.failNext = nil
	}
	started := c.started
	blockNext := c.blockNext
	if blockNext != nil {
		c.started = nil
		c.blockNext = nil
	}
	c.mu.Unlock()

	if blockNext != nil {
		if started != nil {
			close(started)
		}
		<-blockNext
	}
	return err
}

func (c *regulatorTestController) block() (started, release chan struct{}) {
	started = make(chan struct{})
	release = make(chan struct{})
	c.mu.Lock()
	c.started = started
	c.blockNext = release
	c.mu.Unlock()
	return started, release
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

func (c *regulatorTestController) failAlways(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.failAll = err
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

func assertBatteryPowerCycleSnapshot(t *testing.T, logs string, cycle uint64, phase string, command float64, reason string) {
	t.Helper()

	prefix := fmt.Sprintf("battery power control: cycle=%d phase=%s", cycle, phase)
	var snapshot string
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, prefix) {
			snapshot = line
			break
		}
	}
	require.NotEmpty(t, snapshot, "missing cycle snapshot %q", prefix)
	assert.Contains(t, snapshot, " TRACE ")
	assert.Contains(t, snapshot, fmt.Sprintf("command=%.0fW", command))
	assert.Contains(t, snapshot, fmt.Sprintf("last-action=%q", reason))
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
	t.Run("charge uses quarter residual power", func(t *testing.T) {
		f := newRegulatorTestFixture(t, -300, 0, 200)

		f.step(0)

		assert.Equal(t, []float64{-168}, f.controller.values())
		assert.Equal(t, batteryPowerCharging, f.regulator.phase)
	})

	t.Run("discharge prefers small export", func(t *testing.T) {
		f := newRegulatorTestFixture(t, 300, 0, 100)

		f.step(0)

		assert.Equal(t, []float64{235}, f.controller.values())
		assert.Equal(t, batteryPowerDischarging, f.regulator.phase)
	})

	t.Run("negative residual does not start charge on import", func(t *testing.T) {
		f := newRegulatorTestFixture(t, 50, 0, -100)

		f.step(0)

		assert.Empty(t, f.controller.values())
	})
}

func TestBatteryPowerRegulatorConvergesOnLoadStep(t *testing.T) {
	f := newRegulatorTestFixture(t, 4500, 0, 100)

	f.step(0)
	require.Equal(t, []float64{2000}, f.controller.values())

	for _, tc := range []struct {
		battery float64
		grid    float64
		command float64
	}{
		{2000, 2470, 4520},
		{4520, 470, 4868},
		{4868, 122, 4983},
		{4983, 7, 4983},
		{4983, -50, 4983},
	} {
		f.battery.set(tc.battery, nil)
		f.grid.set(tc.grid, nil)
		f.step(batteryPowerControlInterval)
		commands := f.controller.values()
		assert.Equal(t, tc.command, commands[len(commands)-1])
	}
}

func TestBatteryPowerRegulatorAcquiresThroughObservedNeutral(t *testing.T) {
	f := newRegulatorTestFixture(t, 2970, -2000, 100)

	f.step(0)
	assert.Empty(t, f.controller.values())

	f.battery.set(0, nil)
	f.step(batteryPowerControlInterval)
	assert.Equal(t, []float64{3020}, f.controller.values())
}

func TestBatteryPowerRegulatorAcknowledgesBeforeIncrease(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)

	f.step(0)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{-1500}, f.controller.values())

	f.battery.set(-500, nil)
	f.step(5 * time.Second)

	// The command is acknowledged via movement (clearing pending, avoiding a
	// timeout), but the stateless anti-windup gate still refuses a further
	// increase while feedback materially trails the applied -1500W.
	assert.Equal(t, []float64{-1500}, f.controller.values())

	f.battery.set(-1500, nil)
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

func TestBatteryPowerIncreaseParameters(t *testing.T) {
	for _, tc := range []struct {
		name                string
		direction           batteryPowerPhase
		gridPower           float64
		fastImportConfirmed bool
		gain                float64
		maxStep             float64
	}{
		{"discharge below import threshold", batteryPowerDischarging, 499, true, batteryPowerGain, batteryPowerMaxIncreaseStep},
		{"discharge at import threshold", batteryPowerDischarging, 500, true, batteryPowerGain, batteryPowerMaxIncreaseStep},
		{"first discharge import sample", batteryPowerDischarging, 501, false, batteryPowerFastDischargeGain, batteryPowerFastDischargeFirstStep},
		{"confirmed discharge import", batteryPowerDischarging, 501, true, batteryPowerFastDischargeGain, batteryPowerFastDischargeMaxStep},
		{"charge above import threshold", batteryPowerCharging, 501, true, batteryPowerGain, batteryPowerMaxIncreaseStep},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gain, maxStep := batteryPowerIncreaseParameters(tc.direction, tc.gridPower, tc.fastImportConfirmed)
			assert.Equal(t, tc.gain, gain)
			assert.Equal(t, tc.maxStep, maxStep)
		})
	}
}

func TestBatteryPowerIncreasedCommand(t *testing.T) {
	for _, tc := range []struct {
		name           string
		direction      batteryPowerPhase
		gridPower      float64
		rawError       float64
		chargeLimit    float64
		dischargeLimit float64
		fastConfirmed  bool
		expected       float64
	}{
		{"normal discharge gain", batteryPowerDischarging, 500, 520, 5000, 5000, true, 348},
		{"fast discharge gain", batteryPowerDischarging, 501, 521, 5000, 5000, false, 521},
		{"first fast discharge step cap", batteryPowerDischarging, 3000, 3020, 5000, 5000, false, 2000},
		{"confirmed fast discharge step cap", batteryPowerDischarging, 5000, 5020, 6000, 6000, true, 4000},
		{"charge step cap unchanged", batteryPowerCharging, -3000, -2950, 5000, 5000, true, -1500},
		{"fast discharge respects power limit", batteryPowerDischarging, 3000, 3020, 5000, 1200, true, 1200},
		{"charge respects power limit", batteryPowerCharging, -3000, -2950, 1000, 5000, true, -1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &batteryPowerRegulator{
				policy: batteryPowerControlPolicy{
					chargeLimit:    tc.chargeLimit,
					dischargeLimit: tc.dischargeLimit,
				},
			}

			command, ok := r.increasedCommandLocked(tc.direction, tc.gridPower, tc.rawError, tc.fastConfirmed)
			require.True(t, ok)
			assert.Equal(t, tc.expected, command)
		})
	}
}

func TestBatteryPowerRegulatorSustainedImportOverridesPendingReduction(t *testing.T) {
	f := newRegulatorTestFixture(t, 3000, 0, 100)
	f.step(0)
	f.battery.set(2000, nil)
	f.grid.set(1950, nil)
	f.step(batteryPowerControlInterval)
	f.battery.set(4000, nil)
	f.grid.set(-50, nil)
	f.step(batteryPowerControlInterval)
	f.grid.set(-2050, nil)
	f.step(batteryPowerControlInterval)
	require.Equal(t, []float64{2000, 4000, 2000}, f.controller.values())
	require.NotNil(t, f.regulator.pendingCommand)

	f.grid.set(3000, nil)
	f.step(0)
	assert.Equal(t, []float64{2000, 4000, 2000}, f.controller.values(), "one import sample must preserve the pending reduction")

	f.step(batteryPowerControlInterval)
	assert.Equal(t, []float64{2000, 4000, 2000, 5000}, f.controller.values())
	require.NotNil(t, f.regulator.pendingCommand)
	assert.Equal(t, 2000.0, f.regulator.pendingCommand.PreviousCommand)
	assert.Equal(t, 5000.0, f.regulator.pendingCommand.Command)
}

func TestBatteryPowerRegulatorSustainedImportPreservesPendingIncrease(t *testing.T) {
	f := newRegulatorTestFixture(t, 3000, 0, 100)
	f.step(0)
	f.step(batteryPowerControlInterval)

	assert.Equal(t, []float64{2000}, f.controller.values())
	require.NotNil(t, f.regulator.pendingCommand)
	assert.Equal(t, 2000.0, f.regulator.pendingCommand.Command)
	assert.Equal(t, f.clock.Now(), f.regulator.lastFastImportAt)
}

func TestBatteryPowerRegulatorFastImportConfirmationResets(t *testing.T) {
	f := newRegulatorTestFixture(t, 3000, 0, 100)
	f.step(0)

	f.battery.set(2000, nil)
	f.grid.set(0, nil)
	f.step(batteryPowerControlInterval)
	require.Nil(t, f.regulator.pendingCommand)

	f.grid.set(3000, nil)
	f.step(batteryPowerControlInterval)

	assert.Equal(t, []float64{2000, 4000}, f.controller.values(), "import after a calm sample must use the first-sample cap")
}

func TestBatteryPowerRegulatorSlowReadBreaksFastImportConfirmation(t *testing.T) {
	f := newRegulatorTestFixture(t, 3000, 0, 100)
	f.step(0)

	f.regulator.battery.meter = &delayedRegulatorTestMeter{
		clock: f.clock,
		delay: 16 * time.Second,
		power: 2000,
	}
	f.step(batteryPowerControlInterval)

	assert.Equal(t, []float64{2000, 4000}, f.controller.values(), "a delayed sample must use the first-sample cap")
}

func TestBatteryPowerRegulatorImportOverrideWriteFailureStopsAndFaults(t *testing.T) {
	f := newRegulatorTestFixture(t, 3000, 0, 100)
	f.step(0)
	f.battery.set(2000, nil)
	f.grid.set(1950, nil)
	f.step(batteryPowerControlInterval)
	f.battery.set(4000, nil)
	f.grid.set(-50, nil)
	f.step(batteryPowerControlInterval)
	f.grid.set(-2050, nil)
	f.step(batteryPowerControlInterval)

	f.grid.set(3000, nil)
	f.step(0)
	f.controller.fail(errors.New("override failed"))
	f.step(batteryPowerControlInterval)

	assert.Equal(t, []float64{2000, 4000, 2000, 5000, 0}, f.controller.values())
	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
}

func TestBatteryPowerCommandMaterial(t *testing.T) {
	for _, tc := range []struct {
		name     string
		command  float64
		baseline float64
		material bool
	}{
		{"just below the 250W floor stays immaterial", 1000, 751, false},
		{"at the 250W floor is material", 1000, 750, true},
		{"tiny command near a zero baseline stays immaterial", -113, 0, false},
		{"percentage dominates above the floor but stays immaterial", 5000, 4501, false},
		{"percentage boundary above the floor is material", 5000, 4500, true},
		{"high-output noise-level correction stays immaterial", -4837, -4753, false},
		{"low-power reduction close to baseline stays immaterial", 202, 316, false},
		{"genuine saturation increase is material", -4500, -2952, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.material, batteryPowerCommandMaterial(tc.command, tc.baseline))
		})
	}
}

func TestBatteryPowerReductionResponded(t *testing.T) {
	for _, tc := range []struct {
		name         string
		command      float64
		previous     float64
		batteryPower float64
		responded    bool
	}{
		{"discharge settled inside material floor", 1669, 1756, 1720, true},
		{"discharge unchanged at previous command", 1669, 1756, 1756, false},
		{"discharge remains stronger than previous command", 1669, 1756, 1850, false},
		{"discharge residual gap remains material", 1000, 2000, 1500, false},
		{"charge settled inside material floor", -1669, -1756, -1720, true},
		{"charge unchanged at previous command", -1669, -1756, -1756, false},
		{"charge remains stronger than previous command", -1669, -1756, -1850, false},
		{"charge residual gap remains material", -1000, -2000, -1500, false},
		{"material floor boundary stays pending", 1000, 1300, 1250, false},
		{"inside material floor acknowledges", 1000, 1300, 1249, true},
		{"percentage boundary stays pending", 5000, 6000, 5500, false},
		{"inside percentage boundary acknowledges", 5000, 6000, 5499, true},
		{"magnitude increase is not a reduction response", 1500, 1000, 1250, false},
		{"zero command is not a reduction response", 0, 1000, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pending := &pendingBatteryPowerCommand{
				PreviousCommand: tc.previous,
				Command:         tc.command,
			}
			assert.Equal(t, tc.responded, batteryPowerReductionResponded(pending, tc.batteryPower))
		})
	}
}

func TestBatteryPowerRegulatorAcknowledgesSettledReductions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		command  float64
		previous float64
		baseline float64
		final    float64
	}{
		{"10:35 discharge retreat", 1669, 1756, 2238, 1720},
		{"14:20 discharge retreat", 407, 483, 1040, 477},
		{"14:53 discharge retreat", 1170, 1333, 1490, 1264},
		{"mirrored charge retreat", -1669, -1756, -2238, -1720},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRegulatorTestFixture(t, 0, 0, 100)
			appliedAt := f.clock.Now().Add(-batteryPowerControlInterval)
			f.regulator.pendingCommand = &pendingBatteryPowerCommand{
				PreviousCommand: tc.previous,
				Command:         tc.command,
				BaselinePower:   tc.baseline,
				AppliedAt:       appliedAt,
			}

			now := f.clock.Now()
			f.regulator.updateAcknowledgementLocked(batteryPowerSample{
				Value:      tc.previous,
				StartedAt:  now,
				FinishedAt: now,
			})
			require.NotNil(t, f.regulator.pendingCommand, "feedback at the previous command does not prove the reduction")

			delta := tc.command - tc.previous
			tolerance := min(batteryPowerAckTolerance, math.Abs(delta)*batteryPowerAckTolerancePercentage)
			if tc.command < 0 {
				require.Less(t, tc.final, tc.command-tolerance, "test must exercise the relaxed charging path")
			} else {
				require.Greater(t, tc.final, tc.command+tolerance, "test must exercise the relaxed discharging path")
			}

			f.regulator.updateAcknowledgementLocked(batteryPowerSample{
				Value:      tc.final,
				StartedAt:  now,
				FinishedAt: now,
			})
			assert.Nil(t, f.regulator.pendingCommand)
		})
	}
}

func TestBatteryPowerRegulatorSettledReductionUnblocksIncrease(t *testing.T) {
	f := newRegulatorTestFixture(t, 1000, 1756, 100)
	f.regulator.appliedCommand = 1669
	f.regulator.initialized = true
	f.regulator.phase = batteryPowerDischarging
	f.regulator.pendingCommand = &pendingBatteryPowerCommand{
		PreviousCommand: 1756,
		Command:         1669,
		BaselinePower:   2238,
		AppliedAt:       f.clock.Now(),
	}
	f.controller.reset()

	f.step(batteryPowerControlInterval)
	assert.Empty(t, f.controller.values(), "feedback at the previous command must keep the increase blocked")
	require.NotNil(t, f.regulator.pendingCommand)

	f.battery.set(1720, nil)
	f.step(batteryPowerControlInterval)

	commands := f.controller.values()
	require.Len(t, commands, 1, "a settled reduction must release control without waiting for timeout")
	assert.Greater(t, commands[0], 1669.0)
	require.NotNil(t, f.regulator.pendingCommand, "the new material increase must arm its own acknowledgement")
	assert.Equal(t, commands[0], f.regulator.pendingCommand.Command)
}

func TestBatteryPowerRegulatorImmaterialCommandsAccumulateUntilMaterial(t *testing.T) {
	f := newRegulatorTestFixture(t, -250, 0, 200)

	f.step(0)
	assert.Equal(t, []float64{-134}, f.controller.values(), "tiny command still writes normally")
	assert.Nil(t, f.regulator.pendingCommand, "tiny command must not arm acknowledgement")

	// battery shows no response; the control loop keeps writing without proof
	f.step(batteryPowerControlInterval)
	assert.Equal(t, []float64{-134, -268}, f.controller.values(), "unacknowledged command keeps accumulating")
	require.NotNil(t, f.regulator.pendingCommand, "accumulated command-versus-measurement gap must eventually arm")
	assert.Equal(t, -268.0, f.regulator.pendingCommand.Command)
	assert.Equal(t, -134.0, f.regulator.pendingCommand.PreviousCommand)
	assert.Equal(t, 0.0, f.regulator.pendingCommand.BaselinePower)

	// once armed, the existing timeout/stop/cooldown path still applies unchanged
	f.timeoutPendingCommand()
	assert.Equal(t, []float64{-134, -268, 0}, f.controller.values())
	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.False(t, f.regulator.chargeBlockedUntil.IsZero(), "material timeout must still cool down")
}

func TestBatteryPowerRegulatorHighOutputSmallCorrectionStaysImmaterial(t *testing.T) {
	f := newRegulatorTestFixture(t, 0, 0, 100)
	f.regulator.appliedCommand = -4750
	f.regulator.initialized = true
	f.regulator.phase = batteryPowerCharging
	f.controller.reset()

	f.battery.set(-4760, nil)
	f.grid.set(-144, nil)
	f.step(batteryPowerControlInterval)

	assert.Equal(t, []float64{-4830}, f.controller.values(), "correction still writes despite being immaterial")
	assert.Nil(t, f.regulator.pendingCommand, "a small correction close to a high-output baseline must not arm acknowledgement")

	// hold grid and battery steady well past the usual acknowledgement window;
	// nothing may fault even though the correction was never proven
	for range int(batteryPowerMaxSettleTime/batteryPowerControlInterval) + 2 {
		f.step(batteryPowerControlInterval)
	}
	assert.NotContains(t, f.controller.values(), 0.0, "no phantom zero-write from an unacknowledged immaterial command")
	assert.Equal(t, batteryPowerCharging, f.regulator.phase)
	assert.True(t, f.regulator.chargeBlockedUntil.IsZero())
}

func TestBatteryPowerRegulatorLowPowerReductionDoesNotTimeOut(t *testing.T) {
	f := newRegulatorTestFixture(t, 0, 0, 100)
	f.regulator.appliedCommand = 271
	f.regulator.initialized = true
	f.regulator.phase = batteryPowerDischarging
	f.controller.reset()

	f.battery.set(316, nil)
	f.grid.set(-119, nil)
	f.step(batteryPowerControlInterval)

	assert.Equal(t, []float64{202}, f.controller.values(), "grid safety retreat still writes")
	assert.Nil(t, f.regulator.pendingCommand, "a low-power reduction close to baseline must not arm acknowledgement")

	// grid settles right after the retreat; the never-acknowledged 202W
	// reduction must not time out within the usual acknowledgement window
	f.grid.set(0, nil)
	f.waitBeforePendingCommandTimeout()
	assert.Equal(t, []float64{202}, f.controller.values(), "no phantom timeout for an immaterial reduction")
	assert.Equal(t, batteryPowerDischarging, f.regulator.phase)
	assert.True(t, f.regulator.dischargeBlockedUntil.IsZero())
}

// TestBatteryPowerRegulatorEstablishedChargingSaturationHolds reproduces a
// genuine BMS taper: charging is already established with real measured
// power, a further material increase is armed, but the battery collapses
// well below the command instead of proving it. Because the response stays
// on the charging side (not materially discharging), this is now a safe
// saturation hold instead of a fault, and the anti-windup gate prevents a
// further increase until feedback catches up.
func TestBatteryPowerRegulatorEstablishedChargingSaturationHolds(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.regulator.policyMaxAge = time.Hour

	f.step(0)
	assert.Equal(t, []float64{-1500}, f.controller.values())

	f.battery.set(-500, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{-1500}, f.controller.values(), "stateless gate blocks while feedback trails -1500W")
	assert.Nil(t, f.regulator.pendingCommand)

	f.battery.set(-1500, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{-1500, -3000}, f.controller.values())
	require.NotNil(t, f.regulator.pendingCommand, "the -3000W step is itself material and stays pending until proven")

	// let the battery catch up and the grid settle so the -3000W step is
	// acknowledged and pending is cleared before the saturation event
	f.battery.set(-2952, nil)
	f.grid.set(-25, nil)
	f.step(batteryPowerControlInterval)
	assert.Equal(t, []float64{-1500, -3000}, f.controller.values())
	assert.Nil(t, f.regulator.pendingCommand)

	// genuine saturation: the battery barely lags the previous command, but
	// the grid still demands the maximum further increase
	f.grid.set(-6000, nil)
	f.step(5 * time.Second)

	require.Equal(t, []float64{-1500, -3000, -4500}, f.controller.values())
	require.NotNil(t, f.regulator.pendingCommand, "a material increase must still arm acknowledgement")
	assert.Equal(t, -2952.0, f.regulator.pendingCommand.BaselinePower)

	// the battery never proves the increase and later collapses further away,
	// but stays on the charging side: a safe saturation hold, not a fault
	f.battery.set(-873, nil)
	f.timeoutPendingCommand()

	assert.Equal(t, []float64{-1500, -3000, -4500}, f.controller.values(), "no zero write on saturation hold")
	assert.Equal(t, batteryPowerCharging, f.regulator.phase, "must remain in charging phase")
	assert.Equal(t, -4500.0, f.regulator.appliedCommand, "applied command stays held")
	assert.Nil(t, f.regulator.pendingCommand, "pending acknowledgement is cleared")
	assert.True(t, f.regulator.chargeBlockedUntil.IsZero(), "a saturation hold must not start a cooldown")

	// grid demand persists and the battery is still far below the applied
	// command: the stateless anti-windup gate must hold, not escalate
	// further. A periodic unchanged-command refresh may still resend the
	// same value.
	f.grid.set(-6000, nil)
	f.step(batteryPowerControlInterval)
	commands := f.controller.values()
	assert.Equal(t, -4500.0, commands[len(commands)-1], "no further increase while feedback trails")
	assert.Equal(t, batteryPowerCharging, f.regulator.phase)
	assert.Nil(t, f.regulator.pendingCommand)

	// once feedback catches up, a further increase is permitted again
	f.battery.set(-4400, nil)
	f.step(batteryPowerControlInterval)
	commands = f.controller.values()
	assert.Less(t, commands[len(commands)-1], -4500.0, "feedback catching up must permit a later increase")
}

func TestBatteryChargeFeedbackTrails(t *testing.T) {
	for _, tc := range []struct {
		name     string
		command  float64
		battery  float64
		trailing bool
	}{
		{"discharge command never trails", 1500, 0, false},
		{"caught up at command is not trailing", -4500, -4500, false},
		{"exceeding the command is not trailing", -4500, -4800, false},
		{"just below the material floor stays caught up", -1000, -751, false},
		{"at the material floor is trailing", -1000, -750, true},
		{"genuine saturation collapse is trailing", -4500, -873, true},
		{"small positive reading within neutral tolerance is taper, still trailing", -4500, 300, true},
		{"positive reading beyond neutral tolerance is wrong direction, not trailing", -4500, 301, false},
		{"large material discharging is wrong direction, not trailing", -4500, 900, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.trailing, batteryChargeFeedbackTrails(tc.command, tc.battery))
		})
	}
}

// TestBatteryChargeFeedbackCaughtUp proves the anti-windup release gate only
// fires on genuine catch-up: it must not be fooled by a materially
// wrong-direction reading (the battery actually discharging) into treating
// that as "caught up" and permitting an escalated write.
func TestBatteryChargeFeedbackCaughtUp(t *testing.T) {
	for _, tc := range []struct {
		name     string
		command  float64
		battery  float64
		caughtUp bool
	}{
		{"caught up at command", -4500, -4500, true},
		{"exceeded the command", -4500, -4800, true},
		{"immaterial gap counts as caught up", -1000, -751, true},
		{"material gap is not caught up", -1000, -750, false},
		{"genuine saturation collapse is not caught up", -4500, -873, false},
		{"small positive taper reading is not caught up", -4500, 300, false},
		{"wrong-direction reading is never caught up", -4500, 301, false},
		{"large material discharging is never caught up", -4500, 900, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.caughtUp, batteryChargeFeedbackCaughtUp(tc.command, tc.battery))
		})
	}
}

// TestBatteryPowerRegulatorAntiWindupBlocksOnWrongDirectionFeedback proves
// that established charging with no pending command in flight hard-stops
// immediately, rather than sitting silently blocked, when feedback turns
// materially wrong-direction: the anti-windup gate must never be mistaken
// for a safe hold on a bad reading.
func TestBatteryPowerRegulatorAntiWindupBlocksOnWrongDirectionFeedback(t *testing.T) {
	f := newRegulatorTestFixture(t, 0, 0, 0)
	f.regulator.appliedCommand = -4500
	f.regulator.initialized = true
	f.regulator.phase = batteryPowerCharging
	f.controller.reset()

	// the battery reports material discharging while still under a
	// charging command, and grid demand still calls for more charging
	f.battery.set(900, nil)
	f.grid.set(-6000, nil)
	f.step(batteryPowerControlInterval)

	commands := f.controller.values()
	require.NotEmpty(t, commands, "wrong-direction feedback must hard-stop, not sit silently blocked")
	assert.Equal(t, 0.0, commands[len(commands)-1], "must write zero on wrong-direction hard-stop")
	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Nil(t, f.regulator.pendingCommand)
	assert.False(t, f.regulator.chargeBlockedUntil.IsZero(), "wrong-direction hard-stop must cool down like a failed increase")
}

// TestBatteryPowerRegulatorPostHoldWrongDirectionHardStops proves that
// feedback turning materially wrong-direction after a saturation hold (once
// pending is cleared and no timeout is in flight) still hard-stops, exactly
// like a fresh wrong-direction response, rather than remaining held forever.
func TestBatteryPowerRegulatorPostHoldWrongDirectionHardStops(t *testing.T) {
	f := newRegulatorTestFixture(t, 0, 0, 100)
	f.regulator.appliedCommand = -4633
	f.regulator.initialized = true
	f.regulator.phase = batteryPowerCharging
	f.controller.reset()

	// arm a pending increase that times out into a saturation hold
	f.regulator.pendingCommand = &pendingBatteryPowerCommand{
		PreviousCommand: -4633,
		Command:         -5298,
		BaselinePower:   -4633,
		AppliedAt:       f.clock.Now(),
	}
	f.regulator.appliedCommand = -5298
	f.regulator.lastWriteAt = f.clock.Now()
	f.regulator.neutralRequired = false

	f.battery.set(-4633, nil)
	f.grid.set(-6500, nil)
	f.timeoutPendingCommand()

	require.Nil(t, f.regulator.pendingCommand, "saturation hold must clear pending")
	require.Equal(t, batteryPowerCharging, f.regulator.phase)
	f.controller.reset()

	// feedback now turns materially wrong-direction on a later cycle
	f.battery.set(900, nil)
	f.step(batteryPowerControlInterval)

	commands := f.controller.values()
	require.NotEmpty(t, commands, "wrong-direction feedback after a hold must still hard-stop")
	assert.Equal(t, 0.0, commands[len(commands)-1])
	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.False(t, f.regulator.chargeBlockedUntil.IsZero())
}

// TestBatteryPowerRegulatorAntiWindupBlocksImmediatelyOnTrailingFeedback
// proves the stateless pre-increase gate refuses a charging increase before
// any pending command or timeout even exists, using the live 16:21:18 shape:
// applied -5298W, measured -4633W (materially trailing), while export demand
// would otherwise request -5818W. This must block immediately, not after a
// 30s timeout.
func TestBatteryPowerRegulatorAntiWindupBlocksImmediatelyOnTrailingFeedback(t *testing.T) {
	f := newRegulatorTestFixture(t, 0, 0, 100)
	f.regulator.appliedCommand = -5298
	f.regulator.initialized = true
	f.regulator.phase = batteryPowerCharging
	f.regulator.policy.chargeLimit = 8000 // exceed the already-applied -5298W so a bigger step is not clipped
	f.controller.reset()

	f.battery.set(-4633, nil)
	f.grid.set(-6500, nil) // heavy export: would otherwise demand the max step to -5818W
	f.step(batteryPowerControlInterval)

	assert.Empty(t, f.controller.values(), "no write: feedback materially trails the applied command")
	assert.Nil(t, f.regulator.pendingCommand, "no pending command must be armed on a blocked increase")

	// once feedback catches up, the same demand permits the increase
	f.battery.set(-5250, nil)
	f.step(batteryPowerControlInterval)

	commands := f.controller.values()
	require.NotEmpty(t, commands, "feedback catching up must permit the increase")
	assert.Less(t, commands[len(commands)-1], -5298.0)
}

// TestBatteryPowerRegulatorChargingSaturationHoldReproducesLiveTimeouts
// replays the three live charging timeout shapes that showed genuine BMS
// taper or collapse near saturation under persistent export. Each must
// become a saturation hold: no zero write, no fault, no neutral requirement,
// no cooldown, pending cleared, phase remains charging, and the applied
// command is held.
func TestBatteryPowerRegulatorChargingSaturationHoldReproducesLiveTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		command  float64
		previous float64
		baseline float64
		final    float64
		grid     float64
	}{
		{"10:21:48 taper", -6870, -5370, -3820, -2701, -2755},
		{"10:23:42 collapse", -4500, -3000, -2980, -1268, -4125},
		{"16:21:18 collapse", -5818, -5298, -4633, -393, -4687},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRegulatorTestFixture(t, 0, 0, 100)
			f.regulator.appliedCommand = tc.previous
			f.regulator.initialized = true
			f.regulator.phase = batteryPowerCharging
			f.controller.reset()

			f.regulator.pendingCommand = &pendingBatteryPowerCommand{
				PreviousCommand: tc.previous,
				Command:         tc.command,
				BaselinePower:   tc.baseline,
				AppliedAt:       f.clock.Now(),
			}
			f.regulator.appliedCommand = tc.command
			f.regulator.lastWriteAt = f.clock.Now()
			f.regulator.neutralRequired = false

			f.battery.set(tc.final, nil)
			f.grid.set(tc.grid, nil)
			f.waitBeforePendingCommandTimeout()
			f.step(batteryPowerControlInterval)

			assert.Empty(t, f.controller.values(), "no zero write on saturation hold")
			assert.Equal(t, batteryPowerCharging, f.regulator.phase)
			assert.Equal(t, tc.command, f.regulator.appliedCommand, "applied command stays held")
			assert.Nil(t, f.regulator.pendingCommand)
			assert.False(t, f.regulator.neutralRequired)
			assert.True(t, f.regulator.chargeBlockedUntil.IsZero(), "no cooldown from a saturation hold")

			// a following cycle with unchanged trailing feedback must not
			// escalate further; a periodic unchanged-command refresh may
			// still resend the same value
			f.step(batteryPowerControlInterval)
			commands := f.controller.values()
			if len(commands) > 0 {
				assert.Equal(t, tc.command, commands[len(commands)-1], "anti-windup: no further increase while feedback trails")
			}
		})
	}
}

// TestBatteryPowerRegulatorSaturationHoldAllowsRetreatOnGridImport proves the
// existing immediate safety retreat still fires while feedback is trailing
// the applied charging command, in case delayed command application later
// changes grid conditions.
func TestBatteryPowerRegulatorSaturationHoldAllowsRetreatOnGridImport(t *testing.T) {
	f := newRegulatorTestFixture(t, 0, 0, 100)
	f.regulator.appliedCommand = -4500
	f.regulator.initialized = true
	f.regulator.phase = batteryPowerCharging
	f.controller.reset()

	// grid import appears: the direction is now wrong and must retreat
	// immediately regardless of the trailing feedback
	f.battery.set(-873, nil)
	f.grid.set(500, nil)
	f.step(batteryPowerControlInterval)

	commands := f.controller.values()
	require.NotEmpty(t, commands, "safety retreat must still fire while feedback trails")
	assert.Greater(t, commands[len(commands)-1], -4500.0, "retreat must reduce charging magnitude")
}

// A first magnitude increase from neutral, with no established prior
// charging and no measured response, still hard-stops, cools down, and
// rearms exactly as before; unaffected by the saturation hold, this is
// already covered by TestBatteryPowerRegulatorAcknowledgementTimeout and
// TestBatteryPowerRegulatorImmaterialCommandsAccumulateUntilMaterial.

// TestBatteryPowerRegulatorEstablishedChargingWrongDirectionStillTimesOut
// proves that established charging with a materially wrong-direction
// (discharging) response still hard-stops instead of holding.
func TestBatteryPowerRegulatorEstablishedChargingWrongDirectionStillTimesOut(t *testing.T) {
	f := newRegulatorTestFixture(t, 0, 0, 100)
	f.regulator.appliedCommand = -3000
	f.regulator.initialized = true
	f.regulator.phase = batteryPowerCharging
	f.controller.reset()

	f.regulator.pendingCommand = &pendingBatteryPowerCommand{
		PreviousCommand: -3000,
		Command:         -4500,
		BaselinePower:   -2952,
		AppliedAt:       f.clock.Now(),
	}
	f.regulator.appliedCommand = -4500

	// battery materially discharges instead of charging: wrong direction
	f.battery.set(600, nil)
	f.grid.set(-4000, nil)
	f.waitBeforePendingCommandTimeout()
	f.step(batteryPowerControlInterval)

	assert.Equal(t, []float64{0}, f.controller.values(), "wrong-direction feedback must still stop and fault")
	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.False(t, f.regulator.chargeBlockedUntil.IsZero())
}

// TestBatteryPowerRegulatorDischargeSaturationStillTimesOut proves discharge
// saturation timeout behavior is unaffected by the charging-only saturation
// hold, even when discharge feedback trails the command in the same shape.
func TestBatteryPowerRegulatorDischargeSaturationStillTimesOut(t *testing.T) {
	f := newRegulatorTestFixture(t, 0, 0, 100)
	f.regulator.appliedCommand = 3000
	f.regulator.initialized = true
	f.regulator.phase = batteryPowerDischarging
	f.controller.reset()

	f.regulator.pendingCommand = &pendingBatteryPowerCommand{
		PreviousCommand: 3000,
		Command:         4500,
		BaselinePower:   2952,
		AppliedAt:       f.clock.Now(),
	}
	f.regulator.appliedCommand = 4500

	// discharge trails the command the same way a charging taper would, but
	// discharge is out of scope for the saturation hold
	f.battery.set(873, nil)
	f.grid.set(4000, nil)
	f.waitBeforePendingCommandTimeout()
	f.step(batteryPowerControlInterval)

	assert.Equal(t, []float64{0}, f.controller.values(), "discharge timeout behavior is unchanged")
	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.False(t, f.regulator.dischargeBlockedUntil.IsZero())
}

func TestBatteryPowerRegulatorRollsBackUndemandedIncreaseAtTimeout(t *testing.T) {
	for _, tc := range []struct {
		name         string
		initialGrid  float64
		boundaryGrid float64
		firstCommand float64
	}{
		{"charging", -3100, -75, -1500},
		{"discharging", 3000, 0, 2000},
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

	f.grid.set(-50, nil)
	f.battery.set(2000, nil)
	f.step(batteryPowerControlInterval)
	require.Nil(t, f.regulator.pendingCommand)

	cooldownHistory := f.clock.Now().Add(-time.Second)
	f.regulator.dischargeBlockedUntil = cooldownHistory
	f.grid.set(3000, nil)
	f.step(batteryPowerControlInterval)
	require.Equal(t, []float64{2000, 4000}, f.controller.values())

	f.grid.set(-50, nil)
	f.waitBeforePendingCommandTimeout()
	assert.Equal(t, []float64{2000, 4000}, f.controller.values(), "must preserve the acknowledgement window")

	f.step(batteryPowerControlInterval)
	require.Equal(t, []float64{2000, 4000, 2000}, f.controller.values())
	require.NotNil(t, f.regulator.pendingCommand)
	assert.Equal(t, 4000.0, f.regulator.pendingCommand.PreviousCommand)
	assert.Equal(t, 2000.0, f.regulator.pendingCommand.Command)
	assert.Equal(t, cooldownHistory, f.regulator.dischargeBlockedUntil)
	assert.True(t, f.regulator.chargeBlockedUntil.IsZero())

	f.grid.set(3000, nil)
	f.battery.set(3000, nil)
	f.step(batteryPowerControlInterval)
	assert.Equal(t, []float64{2000, 4000, 2000}, f.controller.values(), "rollback must settle before another increase")
	assert.Equal(t, cooldownHistory, f.regulator.dischargeBlockedUntil)

	f.battery.set(2000, nil)
	f.step(batteryPowerControlInterval)
	assert.Equal(t, []float64{2000, 4000, 2000, 5000}, f.controller.values())
	assert.Equal(t, cooldownHistory, f.regulator.dischargeBlockedUntil, "rollback acknowledgement must not clear cooldown history")
}

func TestBatteryPowerRegulatorRollbackToZeroRequiresNeutral(t *testing.T) {
	f := newRegulatorTestFixture(t, 3000, 0, 100)
	f.step(0)

	f.grid.set(-50, nil)
	f.waitBeforePendingCommandTimeout()
	assert.Equal(t, []float64{2000}, f.controller.values(), "must preserve the acknowledgement window")

	f.step(batteryPowerControlInterval)
	require.Equal(t, []float64{2000, 0}, f.controller.values())
	require.True(t, f.regulator.neutralRequired)

	f.grid.set(2970, nil)
	f.battery.set(500, nil)
	f.step(batteryPowerControlInterval)
	assert.Equal(t, []float64{2000, 0}, f.controller.values())

	f.battery.set(0, nil)
	f.step(batteryPowerControlInterval)
	assert.Equal(t, []float64{2000, 0, 3020}, f.controller.values())
}

func TestBatteryPowerRegulatorRollbackWriteFailureStopsAndFaults(t *testing.T) {
	f := newRegulatorTestFixture(t, 3000, 0, 100)
	f.step(0)

	f.grid.set(-50, nil)
	f.battery.set(2000, nil)
	f.step(batteryPowerControlInterval)
	f.grid.set(3000, nil)
	f.step(batteryPowerControlInterval)
	require.Equal(t, []float64{2000, 4000}, f.controller.values())

	rollbackErr := errors.New("rollback failed")
	f.controller.fail(rollbackErr)
	f.grid.set(-50, nil)
	f.waitBeforePendingCommandTimeout()
	f.step(batteryPowerControlInterval)

	assert.Equal(t, []float64{2000, 4000, 2000, 0}, f.controller.values())
	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, 0.0, f.regulator.appliedCommand)
	assert.Nil(t, f.regulator.pendingCommand)
}

func TestBatteryPowerRegulatorSafetyRetreatPrecedesTimeoutRollback(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)
	f.waitBeforePendingCommandTimeout()

	f.grid.set(1025, nil)
	f.step(batteryPowerControlInterval)

	assert.Equal(t, []float64{-1500, -450}, f.controller.values())
	assert.Equal(t, batteryPowerCharging, f.regulator.phase)
	require.NotNil(t, f.regulator.pendingCommand)
	assert.Equal(t, -450.0, f.regulator.pendingCommand.Command)
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

		f.grid.set(1025, nil)
		reads := f.battery.readCount()
		f.step(5 * time.Second)

		assert.Equal(t, []float64{-1500, -450}, f.controller.values())
		require.NotNil(t, f.regulator.pendingCommand)
		assert.Equal(t, reads+1, f.battery.readCount(), "every cycle must use fresh battery feedback")

		f.grid.set(-975, nil)
		f.battery.set(-1000, nil)
		f.step(5 * time.Second)
		assert.Equal(t, []float64{-1500, -450}, f.controller.values(), "must not increase before the retreat settles")

		f.battery.set(-600, nil)
		f.step(5 * time.Second)
		assert.Equal(t, []float64{-1500, -450, -1087}, f.controller.values())
	})

	t.Run("discharging", func(t *testing.T) {
		f := newRegulatorTestFixture(t, 3000, 0, 100)
		f.step(0)

		f.grid.set(-1030, nil)
		f.step(batteryPowerControlInterval)
		assert.Equal(t, []float64{2000, 1020}, f.controller.values())
		require.NotNil(t, f.regulator.pendingCommand)

		f.grid.set(970, nil)
		f.battery.set(1300, nil)
		f.step(batteryPowerControlInterval)
		assert.Equal(t, []float64{2000, 1020}, f.controller.values(), "must not increase before the retreat settles")

		f.battery.set(1100, nil)
		f.step(batteryPowerControlInterval)
		assert.Equal(t, []float64{2000, 1020, 2040}, f.controller.values())
	})
}

func TestBatteryPowerRegulatorCycleDiagnostics(t *testing.T) {
	t.Run("active retreat", func(t *testing.T) {
		f := newRegulatorTestFixture(t, -3100, 0, 100)
		f.step(0)

		var logs bytes.Buffer
		f.regulator.log.SetLogOutput(&logs)
		f.grid.set(1025, nil)
		f.step(batteryPowerControlInterval)

		out := logs.String()
		assertBatteryPowerCycleSnapshot(t, out, 2, "charging", -1500, "acknowledged bounded correction")
		assert.Contains(t, out, "battery power control: phase=charging command=-450W grid-target=-25W reason=raw grid safety retreat cycle=2")
	})

	t.Run("reversal neutral barrier", func(t *testing.T) {
		f := newRegulatorTestFixture(t, -3100, 0, 100)
		f.step(0)
		f.battery.set(-1500, nil)
		f.step(batteryPowerControlInterval)
		f.grid.set(4000, nil)
		f.step(batteryPowerControlInterval)
		require.Equal(t, []float64{-1500, -3000, 0}, f.controller.values())

		var logs bytes.Buffer
		f.regulator.log.SetLogOutput(&logs)
		f.battery.set(-500, nil)
		f.step(batteryPowerControlInterval)
		f.battery.set(0, nil)
		f.step(batteryPowerControlInterval)

		out := logs.String()
		assertBatteryPowerCycleSnapshot(t, out, 4, "neutral", 0, "raw grid safety retreat cycle=3")
		assertBatteryPowerCycleSnapshot(t, out, 5, "neutral", 0, "raw grid safety retreat cycle=3")
		assert.Equal(t, []float64{-1500, -3000, 0, 4000}, f.controller.values())
	})
}

func TestBatteryPowerRegulatorAllowsFurtherSafetyRetreat(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	f.grid.set(525, nil)
	f.step(5 * time.Second)
	require.Equal(t, []float64{-1500, -950}, f.controller.values())

	f.grid.set(225, nil)
	f.battery.set(-1500, nil)
	f.step(5 * time.Second)

	assert.Equal(t, []float64{-1500, -950, -700}, f.controller.values())
}

func TestBatteryPowerRegulatorSafetyRetreatTimeout(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	f.grid.set(1025, nil)
	f.step(5 * time.Second)
	require.Equal(t, []float64{-1500, -450}, f.controller.values())

	f.grid.set(-75, nil)
	f.battery.set(-1500, nil)
	var logs bytes.Buffer
	f.regulator.log.SetLogOutput(&logs)
	cooldownHistory := f.clock.Now().Add(-time.Second)
	f.regulator.chargeBlockedUntil = cooldownHistory
	for range 6 {
		f.step(5 * time.Second)
	}

	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, []float64{-1500, -450, 0}, f.controller.values())
	assert.Equal(t, cooldownHistory, f.regulator.chargeBlockedUntil)
	assert.True(t, f.regulator.dischargeBlockedUntil.IsZero())
	assert.Contains(t, logs.String(), "command acknowledgement timed out: direction=charging command=-450W previous=-1500W")
	assert.NotContains(t, logs.String(), "repeated command acknowledgement timeout")
}

func TestBatteryPowerRegulatorRetreatSnapsSmallCommandToZero(t *testing.T) {
	f := newRegulatorTestFixture(t, -475, 0, 100)
	f.step(0)
	require.Equal(t, []float64{-302}, f.controller.values())

	f.grid.set(255, nil)
	f.battery.set(-100, nil)
	f.step(5 * time.Second)

	assert.Equal(t, []float64{-302, 0}, f.controller.values())
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
	t.Run("charging", func(t *testing.T) {
		f := newRegulatorTestFixture(t, -100, 0, 200)
		f.step(0)
		assert.Empty(t, f.controller.values())

		f.grid.set(-101, nil)
		f.step(5 * time.Second)

		assert.Equal(t, []float64{-34}, f.controller.values())
		assert.Equal(t, batteryPowerCharging, f.regulator.phase)
	})

	t.Run("discharging", func(t *testing.T) {
		f := newRegulatorTestFixture(t, 50, 0, 100)
		f.step(0)
		assert.Empty(t, f.controller.values())

		f.grid.set(51, nil)
		f.step(5 * time.Second)

		assert.Equal(t, []float64{68}, f.controller.values())
		assert.Equal(t, batteryPowerDischarging, f.regulator.phase)
	})
}

func TestBatteryPowerRegulatorCorrectsLowPowerImport(t *testing.T) {
	f := newRegulatorTestFixture(t, 300, 0, 100)
	f.step(0)
	require.Equal(t, []float64{235}, f.controller.values())

	f.grid.set(80, nil)
	f.battery.set(0, nil) // far enough from the command to stay material
	f.step(5 * time.Second)
	assert.Equal(t, []float64{235, 322}, f.controller.values())

	f.step(5 * time.Second)
	assert.Equal(t, []float64{235, 322}, f.controller.values(), "must wait for low-power feedback")

	f.grid.set(0, nil)
	f.battery.set(322, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{235, 322}, f.controller.values())
}

func TestBatteryPowerRegulatorUsesBiasedDischargeDeadband(t *testing.T) {
	f := newRegulatorTestFixture(t, 300, 0, 100)
	f.step(0)
	require.Equal(t, []float64{235}, f.controller.values())

	f.grid.set(0, nil)
	f.battery.set(180, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{235}, f.controller.values(), "zero grid power is inside the deadband")

	f.grid.set(1, nil)
	f.step(5 * time.Second)
	require.Equal(t, []float64{235, 269}, f.controller.values(), "import beyond the deadband must increase discharge")

	f.grid.set(-100, nil)
	f.battery.set(269, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{235, 269}, f.controller.values(), "100W export is inside the deadband")

	f.grid.set(-101, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{235, 269, 218}, f.controller.values(), "export beyond the deadband must retreat")
}

func TestBatteryPowerRegulatorAcknowledgesSmallCorrectionMovement(t *testing.T) {
	f := newRegulatorTestFixture(t, 270, 0, 100)
	f.step(0)
	require.Equal(t, []float64{214}, f.controller.values())

	f.grid.set(0.2, nil)
	f.battery.set(-100, nil) // far enough from the command to stay material
	f.step(5 * time.Second)
	require.Equal(t, []float64{214, 248}, f.controller.values())

	f.step(5 * time.Second)
	assert.Equal(t, []float64{214, 248}, f.controller.values(), "unchanged feedback must not acknowledge")

	f.battery.set(141, nil)
	f.step(5 * time.Second)
	assert.Equal(t, []float64{214, 248, 282}, f.controller.values(), "directional movement must acknowledge")
}

func TestBatteryPowerRegulatorWaitsForLowPowerRetreat(t *testing.T) {
	f := newRegulatorTestFixture(t, 270, 0, 100)
	f.step(0)
	require.Equal(t, []float64{214}, f.controller.values())

	f.grid.set(-110, nil)
	f.battery.set(500, nil) // far enough from the retreat target to stay material
	f.step(5 * time.Second)
	require.Equal(t, []float64{214, 154}, f.controller.values())

	f.grid.set(50, nil)
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

	f.clock.Add(batteryPowerControlInterval + batteryPowerControlOffset - 500*time.Millisecond)
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
	f := newRegulatorTestFixture(t, -475, 0, 100)

	f.step(0)
	assert.Equal(t, []float64{-302}, f.controller.values())
}

func TestBatteryPowerRegulatorObservedZeroBeforeReversal(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	f.battery.set(-1500, nil)
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
	assert.Equal(t, []float64{-1500, -3000, 0, 2000}, f.controller.values())
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

	// acknowledged via movement, avoiding a timeout, but the stateless
	// anti-windup gate still refuses a further increase while feedback
	// materially trails the applied -1500W
	assert.NotEqual(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, []float64{-1500}, f.controller.values())

	f.battery.set(-1500, nil)
	f.step(5 * time.Second)

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

	assert.Equal(t, []float64{-1500, 0, 2000}, f.controller.values())
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
		PreviousCommand: -500,
		Command:         -400,
		BaselinePower:   -1000,
		AppliedAt:       f.clock.Now().Add(-batteryPowerControlInterval),
	}

	now := f.clock.Now()
	f.regulator.updateAcknowledgementLocked(batteryPowerSample{
		Value:      -451,
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
	f := newRegulatorTestFixture(t, 270, 0, 100)
	f.step(0)
	require.Equal(t, []float64{214}, f.controller.values())

	f.regulator.battery.meter = &delayedRegulatorTestMeter{
		clock: f.clock,
		delay: 16 * time.Second,
		power: 150,
	}
	f.grid.set(50, nil)
	gridReads := f.grid.readCount()
	f.step(5 * time.Second)

	assert.Equal(t, batteryPowerDischarging, f.regulator.phase)
	assert.Equal(t, []float64{214, 281}, f.controller.values(), "fresh feedback must resume control after a slow read")
	assert.Equal(t, gridReads+1, f.grid.readCount(), "grid must be read after the slow battery response")
}

func TestBatteryPowerRegulatorRecoversFeedbackDuringGrace(t *testing.T) {
	f := newRegulatorTestFixture(t, 270, 0, 100)
	f.step(0)
	require.Equal(t, []float64{214}, f.controller.values())

	f.battery.set(0, errors.New("read failed"))
	f.grid.set(10, nil)
	f.step(5 * time.Second)
	require.Equal(t, []float64{214}, f.controller.values())

	f.battery.set(214, nil)
	f.step(5 * time.Second)

	assert.Equal(t, batteryPowerDischarging, f.regulator.phase)
	assert.Equal(t, []float64{214, 254}, f.controller.values())
}

func TestBatteryPowerRegulatorFeedbackGraceExpires(t *testing.T) {
	f := newRegulatorTestFixture(t, 270, 0, 100)
	f.step(0)
	require.Equal(t, []float64{214}, f.controller.values())

	f.battery.set(0, errors.New("read failed"))
	f.grid.set(10, nil)
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
	require.Equal(t, []float64{2000}, f.controller.values())

	f.battery.set(0, errors.New("read failed"))
	f.grid.set(-1030, nil)
	f.step(5 * time.Second)
	require.Equal(t, []float64{2000, 1020}, f.controller.values())

	f.grid.set(970, nil)
	f.step(5 * time.Second)

	assert.Equal(t, batteryPowerDischarging, f.regulator.phase)
	assert.Equal(t, []float64{2000, 1020}, f.controller.values(), "feedback grace must not increase power")
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

func TestBatteryPowerRegulatorSlowWriteDoesNotStaleFreshGrid(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)
	require.Equal(t, batteryPowerCharging, f.regulator.phase)

	var logs bytes.Buffer
	f.regulator.log.SetLogOutput(&logs)

	batteryStarted := make(chan struct{})
	batteryRelease := make(chan struct{})
	gridDone := make(chan struct{})
	f.regulator.battery.meter = &blockingRegulatorTestMeter{
		started: batteryStarted,
		release: batteryRelease,
		power:   -1500,
	}
	f.regulator.gridMeter = &blockingRegulatorTestMeter{
		done:  gridDone,
		power: -3100,
	}

	writeStarted, writeRelease := f.controller.block()

	tickDone := make(chan struct{})
	go func() {
		defer close(tickDone)
		f.regulator.tick()
	}()
	<-batteryStarted

	policyDone := make(chan error, 1)
	go func() {
		policy := f.regulator.policy
		policy.chargeAllowed = false
		policyDone <- f.regulator.setPolicy(policy)
	}()
	<-writeStarted

	close(batteryRelease)
	<-gridDone
	f.clock.Add(batteryPowerControlInterval + time.Second)
	close(writeRelease)

	<-tickDone
	require.NoError(t, <-policyDone)

	assert.NotContains(t, logs.String(), "grid unavailable")
	assert.NotEqual(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, batteryPowerNeutral, f.regulator.phase)
	assert.Equal(t, 0.0, f.regulator.appliedCommand)
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

func TestBatteryPowerRegulatorFailedStartRetriesZero(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.controller.failAlways(errors.New("write failed"))

	f.step(0)

	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, 0.0, f.regulator.appliedCommand)
	assert.True(t, f.regulator.initialized)
	assert.False(t, f.regulator.stopFailureSince.IsZero())
	assert.Equal(t, []float64{-1500, 0}, f.controller.values())

	f.battery.set(-800, nil)
	f.step(batteryPowerControlInterval)

	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, 0.0, f.regulator.appliedCommand)
	assert.Equal(t, []float64{-1500, 0, 0}, f.controller.values(), "failed best-effort zero must be retried")

	require.Error(t, f.regulator.stop())
	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, []float64{-1500, 0, 0, 0}, f.controller.values(), "shutdown must retry zero after a failed stop")
}

func TestBatteryPowerRegulatorFailedStartStopToNeutralRetriesZero(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.controller.failAlways(errors.New("write failed"))
	f.step(0)
	require.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	f.controller.reset()
	f.clock.Add(batteryPowerControlInterval)

	f.regulator.mu.Lock()
	err := f.regulator.stopToNeutralLocked("policy eligibility changed")
	f.regulator.mu.Unlock()
	require.Error(t, err)
	assert.Equal(t, batteryPowerFaultStopping, f.regulator.phase)
	assert.Equal(t, []float64{0}, f.controller.values(), "failed stop must not be treated as already neutral")
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

func TestBatteryPowerRegulatorBacksOffPersistentStopFailure(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	stopErr := errors.New("stop failed")
	f.controller.failAlways(stopErr)
	require.ErrorIs(t, f.regulator.release(), stopErr)

	for range int(batteryPowerStopRetrySafetyWindow/batteryPowerControlInterval) - 1 {
		f.step(batteryPowerControlInterval)
	}
	attemptsDuringSafetyWindow := len(f.controller.values())

	f.step(batteryPowerControlInterval)
	assert.Len(t, f.controller.values(), attemptsDuringSafetyWindow, "must back off after the forced-control safety window")

	f.step(batteryPowerStopRetryInterval - batteryPowerControlInterval)
	assert.Len(t, f.controller.values(), attemptsDuringSafetyWindow+1, "must retain bounded stop retries")
}

func TestBatteryPowerRegulatorBacksOffPolicyStopFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(*batteryPowerRegulator) error
	}{
		{
			"direction disallowed",
			func(r *batteryPowerRegulator) error {
				policy := r.policy
				policy.chargeAllowed = false
				return r.setPolicy(policy)
			},
		},
		{
			"policy released",
			func(r *batteryPowerRegulator) error {
				policy := r.policy
				policy.active = false
				return r.setPolicy(policy)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRegulatorTestFixture(t, -3100, 0, 100)
			f.step(0)

			stopErr := errors.New("stop failed")
			f.controller.failAlways(stopErr)
			require.ErrorIs(t, tc.stop(f.regulator), stopErr)

			f.clock.Add(batteryPowerStopRetrySafetyWindow - batteryPowerControlInterval)
			require.ErrorIs(t, tc.stop(f.regulator), stopErr)
			f.clock.Add(batteryPowerControlInterval)
			attempts := len(f.controller.values())
			require.NoError(t, tc.stop(f.regulator))
			assert.Len(t, f.controller.values(), attempts, "must back off repeated policy stop attempts")

			f.clock.Add(batteryPowerStopRetryInterval - batteryPowerControlInterval)
			require.ErrorIs(t, tc.stop(f.regulator), stopErr)
			assert.Len(t, f.controller.values(), attempts+1, "must retain bounded policy stop retries")
		})
	}
}

func TestBatteryPowerRegulatorModeHandoffUsesStopBackoff(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	stopErr := errors.New("stop failed")
	f.controller.failAlways(stopErr)
	require.ErrorIs(t, f.regulator.releaseForHandoff(), stopErr)

	attempts := len(f.controller.values())
	require.ErrorIs(t, f.regulator.releaseForHandoff(), errBatteryPowerStopRetryPending)
	assert.Len(t, f.controller.values(), attempts, "same-cycle policy update must not duplicate the handoff stop")

	f.clock.Add(batteryPowerControlInterval)
	require.ErrorIs(t, f.regulator.releaseForHandoff(), stopErr)
	assert.Len(t, f.controller.values(), attempts+1, "handoff must retry during the forced-control safety window")

	f.clock.Add(batteryPowerStopRetrySafetyWindow - batteryPowerControlInterval)
	attempts = len(f.controller.values())
	require.ErrorIs(t, f.regulator.releaseForHandoff(), errBatteryPowerStopRetryPending)
	assert.Len(t, f.controller.values(), attempts, "handoff must use bounded retries after the safety window")
}

func TestBatteryPowerRegulatorShutdownBypassesStopBackoff(t *testing.T) {
	f := newRegulatorTestFixture(t, -3100, 0, 100)
	f.step(0)

	stopErr := errors.New("stop failed")
	f.controller.failAlways(stopErr)
	require.ErrorIs(t, f.regulator.release(), stopErr)
	f.clock.Add(batteryPowerStopRetrySafetyWindow)

	attempts := len(f.controller.values())
	require.ErrorIs(t, f.regulator.release(), stopErr)
	assert.Len(t, f.controller.values(), attempts+1)
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

func TestBatteryPowerRegulatorClampsChargeLimitDrop(t *testing.T) {
	f := newRegulatorTestFixture(t, -5000, 0, 100)
	policy := f.regulator.policy
	policy.chargeLimit = 4000
	require.NoError(t, f.regulator.setPolicy(policy))

	f.step(0)
	f.battery.set(-1500, nil)
	f.step(batteryPowerControlInterval)
	f.battery.set(-3000, nil)
	f.step(batteryPowerControlInterval)
	require.Equal(t, []float64{-1500, -3000, -4000}, f.controller.values())
	require.Equal(t, batteryPowerCharging, f.regulator.phase)

	policy = f.regulator.policy
	policy.chargeLimit = 2000
	require.NoError(t, f.regulator.setPolicy(policy))

	assert.Equal(t, []float64{-1500, -3000, -4000, -2000}, f.controller.values())
	assert.Equal(t, batteryPowerCharging, f.regulator.phase)
	assert.Equal(t, -2000.0, f.regulator.appliedCommand)
	assert.False(t, f.regulator.neutralRequired)
}

func TestBatteryPowerRegulatorClampsDischargeLimitDrop(t *testing.T) {
	f := newRegulatorTestFixture(t, 4500, 0, 100)
	policy := f.regulator.policy
	policy.dischargeLimit = 4000
	require.NoError(t, f.regulator.setPolicy(policy))

	f.step(0)
	f.battery.set(2000, nil)
	f.grid.set(2550, nil)
	f.step(batteryPowerControlInterval)
	require.Equal(t, []float64{2000, 4000}, f.controller.values())
	require.Equal(t, batteryPowerDischarging, f.regulator.phase)

	policy = f.regulator.policy
	policy.dischargeLimit = 2000
	require.NoError(t, f.regulator.setPolicy(policy))

	assert.Equal(t, []float64{2000, 4000, 2000}, f.controller.values())
	assert.Equal(t, batteryPowerDischarging, f.regulator.phase)
	assert.Equal(t, 2000.0, f.regulator.appliedCommand)
	assert.False(t, f.regulator.neutralRequired)
}

func TestBatteryPowerRegulatorLimitIncreaseDoesNotJump(t *testing.T) {
	f := newRegulatorTestFixture(t, -5000, 0, 100)
	f.step(0)
	require.Equal(t, []float64{-1500}, f.controller.values())
	require.Equal(t, batteryPowerCharging, f.regulator.phase)

	policy := f.regulator.policy
	policy.chargeLimit = 8000
	require.NoError(t, f.regulator.setPolicy(policy))

	assert.Equal(t, []float64{-1500}, f.controller.values(), "limit increase must not stop or rewrite")
	assert.Equal(t, batteryPowerCharging, f.regulator.phase)
	assert.Equal(t, -1500.0, f.regulator.appliedCommand)

	f.step(batteryPowerControlInterval)
	assert.Equal(t, []float64{-1500}, f.controller.values(), "unacknowledged increase must stay gated")

	f.battery.set(-1500, nil)
	f.step(batteryPowerControlInterval)
	assert.Equal(t, []float64{-1500, -3000}, f.controller.values())
	assert.Equal(t, batteryPowerCharging, f.regulator.phase)
}

func TestBatteryPowerRegulatorDisallowedDirectionStopsToNeutral(t *testing.T) {
	f := newRegulatorTestFixture(t, -5000, 0, 100)
	f.step(0)
	require.Equal(t, []float64{-1500}, f.controller.values())
	require.Equal(t, batteryPowerCharging, f.regulator.phase)

	policy := f.regulator.policy
	policy.chargeAllowed = false
	require.NoError(t, f.regulator.setPolicy(policy))

	assert.Equal(t, []float64{-1500, 0}, f.controller.values())
	assert.Equal(t, batteryPowerNeutral, f.regulator.phase)
	assert.True(t, f.regulator.neutralRequired)
}

func TestBatteryPowerRegulatorForceCharge(t *testing.T) {
	f := newRegulatorTestFixture(t, 3000, 0, 100)

	policy := f.regulator.policy
	policy.forceCharge = true
	require.NoError(t, f.regulator.setPolicy(policy))
	f.step(0)

	assert.Equal(t, []float64{-1500}, f.controller.values())
}

// TestBatteryPowerRegulatorForceChargeSaturationHoldBlocksFurtherRamp proves
// that force charge is subject to the same stateless anti-windup gate as
// normal charging control: an established force-charge increase that times
// out while feedback materially trails becomes a saturation hold, and the
// gate then blocks any further 1500W ramp step until feedback genuinely
// catches up, matching "before every charging magnitude increase".
func TestBatteryPowerRegulatorForceChargeSaturationHoldBlocksFurtherRamp(t *testing.T) {
	f := newRegulatorTestFixture(t, 0, 0, 100)
	f.regulator.policyMaxAge = time.Hour
	policy := f.regulator.policy
	policy.forceCharge = true
	require.NoError(t, f.regulator.setPolicy(policy))
	f.step(0)
	require.Equal(t, []float64{-1500}, f.controller.values(), "fresh start ramps unconditionally")

	// establish charging at -1500, then arm a further force-charge increase
	// to -3000 that will time out with feedback still trailing
	f.regulator.pendingCommand = nil
	f.regulator.appliedCommand = -3000
	f.regulator.lastWriteAt = f.clock.Now()
	f.regulator.neutralRequired = false
	f.regulator.pendingCommand = &pendingBatteryPowerCommand{
		PreviousCommand: -1500,
		Command:         -3000,
		BaselinePower:   -1500,
		AppliedAt:       f.clock.Now(),
	}
	f.battery.set(-1500, nil)
	f.controller.reset()
	f.timeoutPendingCommand()

	assert.Empty(t, f.controller.values(), "no zero write on force-charge saturation hold")
	assert.Equal(t, batteryPowerCharging, f.regulator.phase)
	assert.Equal(t, -3000.0, f.regulator.appliedCommand, "applied command stays held")
	assert.Nil(t, f.regulator.pendingCommand)

	// subsequent cycles must not ramp further while feedback still trails;
	// a periodic unchanged-command refresh may still resend the same value
	f.step(batteryPowerControlInterval)
	f.step(batteryPowerControlInterval)
	for _, c := range f.controller.values() {
		assert.Equal(t, -3000.0, c, "anti-windup: no further force-charge ramp while feedback trails")
	}

	// once feedback catches up, the next bounded increase is permitted
	f.battery.set(-3000, nil)
	f.step(batteryPowerControlInterval)
	commands := f.controller.values()
	require.NotEmpty(t, commands, "feedback catching up must permit the next force-charge ramp")
	assert.Equal(t, -4500.0, commands[len(commands)-1])
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
		batterySocUpdated:     []time.Time{time.Now()},
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

	lp := &Loadpoint{mode: api.ModeNow}
	lp.setStatus(api.StatusC)
	site.loadpoints = []*Loadpoint{lp}
	site.batteryDischargeMode = api.BatteryDischargeReserve
	site.batteryReserveSoc = 20
	site.battery.Soc = 20

	policy = site.batteryPowerControlPolicy(api.Rate{})
	assert.False(t, policy.dischargeAllowed)

	lp.setStatus(api.StatusB)
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
