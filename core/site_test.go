package core

import (
	"errors"
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/types"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

type priorityTestBattery struct {
	power      float64
	soc        float64
	socErr     error
	limitReads int
	socReads   int
	controller *regulatorTestController
}

func (b *priorityTestBattery) CurrentPower() (float64, error) {
	return b.power, nil
}

func (b *priorityTestBattery) Soc() (float64, error) {
	return b.soc, b.socErr
}

func (b *priorityTestBattery) SetBatteryPower(power float64) error {
	return b.controller.SetBatteryPower(power)
}

func (b *priorityTestBattery) GetPowerLimits() (float64, float64) {
	b.limitReads++
	return 5000, 5000
}

func (b *priorityTestBattery) GetSocLimits() (float64, float64) {
	b.socReads++
	return 20, 95
}

type priorityTestModeBattery struct {
	*priorityTestBattery
}

func (b priorityTestModeBattery) BatteryModes() []api.BatteryMode {
	return []api.BatteryMode{api.BatteryNormal, api.BatteryHold, api.BatteryCharge}
}

func (b priorityTestModeBattery) SetBatteryMode(api.BatteryMode) error {
	return nil
}

func continuousPriorityTestSite(t *testing.T, batteryPower, batterySoc float64) (*Site, *clock.Mock) {
	t.Helper()

	return continuousPriorityTestSiteWith(t, batteryPower, batterySoc, func(b *priorityTestBattery) api.Meter { return b })
}

func continuousPriorityTestSiteWith(t *testing.T, batteryPower, batterySoc float64, meter func(*priorityTestBattery) api.Meter) (*Site, *clock.Mock) {
	t.Helper()

	battery := &priorityTestBattery{
		power:      batteryPower,
		soc:        batterySoc,
		controller: &regulatorTestController{},
	}
	bat := meter(battery)
	batteryMeters := []config.Device[api.Meter]{
		config.NewStaticDevice(config.Named{Name: "battery"}, bat),
	}

	clck := clock.NewMock()
	clck.Set(time.Now())
	now := clck.Now()
	regulator := newBatteryPowerRegulator(
		util.NewLogger(t.Name()),
		&regulatorTestMeter{},
		batteryMeters,
	)
	require.NotNil(t, regulator)
	regulator.clock = clck
	regulator.policy = batteryPowerControlPolicy{
		valid:          true,
		active:         true,
		chargeAllowed:  true,
		residualPower:  200,
		chargeLimit:    5000,
		soc:            batterySoc,
		minSoc:         20,
		maxSoc:         95,
		socLimitsValid: true,
		socUpdatedAt:   now,
		updatedAt:      now,
	}
	regulator.phase = batteryPowerCharging
	regulator.appliedCommand = batteryPower
	regulator.initialized = true
	regulator.lastBatterySample = batteryPowerSample{
		Value:      batteryPower,
		StartedAt:  now.Add(-time.Millisecond),
		FinishedAt: now,
	}

	site := &Site{
		log:                   util.NewLogger(t.Name()),
		batteryMeters:         batteryMeters,
		pvMeters:              []config.Device[api.Meter]{},
		prioritySoc:           50,
		ResidualPower:         200,
		batteryMode:           api.BatteryNormal,
		batteryPowerRegulator: regulator,
		batterySocUpdated:     []time.Time{now},
	}
	soc := batterySoc
	site.battery = types.BatteryState{
		Power:   batteryPower,
		Soc:     batterySoc,
		Devices: []types.Measurement{{Soc: &soc}},
	}

	return site, clck
}

func continuousPrioritySitePower(site *Site, flexiblePower float64) sitePowerResult {
	return site.sitePower(site.state(), 0, flexiblePower)
}

func resetContinuousPriorityController(site *Site) {
	regulator := site.batteryPowerRegulator
	regulator.policy = batteryPowerControlPolicy{}
	regulator.phase = batteryPowerReleased
	regulator.appliedCommand = 0
	regulator.initialized = false
	regulator.pendingCommand = nil
	regulator.lastBatterySample = batteryPowerSample{}
}

// TestSitePowerPriorityAdjustment verifies that sitePower returns the adjustment
// applied for battery priority below prioritySoc, such that adding it back yields
// the unadjusted site power for loadpoints with battery boost active (#30541)
func TestSitePowerPriorityAdjustment(t *testing.T) {
	const prioritySoc = 50

	for _, tc := range []struct {
		name                        string
		soc, power, excessDC        float64 // battery
		expSitePower, expAdjustment float64
		expReconstructed            float64 // sitePower + adjustment: the unadjusted site power a boost loadpoint sees
	}{
		// battery priority does not apply: no adjustment
		{"charging above prioritySoc", 80, -2000, 0, -2000, 0, -2000},
		// battery charge power hidden and residual power forced to 100W:
		// adding the adjustment back restores the unadjusted -2000W
		{"charging below prioritySoc", 30, -2000, 0, 100, -2100, -2000},
		// battery not charging: only the forced residual power applies
		{"discharging below prioritySoc", 30, 500, 0, 600, -100, 500},
		// excess DC power can only reach the battery, never the (AC) vehicle, so it
		// must stay netted out of the reconstructed surplus: of 2000W charging with
		// 500W un-redirectable DC excess, only 1500W is available to a boost loadpoint
		{"charging below prioritySoc with excess DC", 30, -2000, 500, 100, -1600, -1500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)

			meter := api.NewMockMeter(ctrl)
			meter.EXPECT().CurrentPower().Return(tc.power, nil).AnyTimes()

			battery := api.NewMockBattery(ctrl)
			battery.EXPECT().Soc().Return(tc.soc, nil).AnyTimes()

			var bat api.Meter = &struct {
				api.Meter
				api.Battery
			}{
				Meter:   meter,
				Battery: battery,
			}

			site := &Site{
				log:           util.NewLogger("foo"),
				batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{}, bat)},
				prioritySoc:   prioritySoc,
			}
			state, err := site.updateMeters()
			require.NoError(t, err)
			state.excessDCPower = tc.excessDC

			res := site.sitePower(state, 0, 0)
			assert.Equal(t, tc.expSitePower, res.power, "sitePower")
			assert.Equal(t, tc.expAdjustment, res.priorityAdjustment, "priority adjustment")
			assert.Equal(t, tc.expReconstructed, res.power+res.priorityAdjustment, "reconstructed (unadjusted) site power")
		})
	}
}

func TestSitePowerBatteryBufferAndStart(t *testing.T) {
	for _, tc := range []struct {
		name         string
		bufferSoc    float64
		startSoc     float64
		soc          float64
		wantBuffered bool
		wantStart    bool
	}{
		{"above buffer and start", 20, 80, 85, true, true},
		{"above buffer below start", 20, 80, 70, true, false},
		{"at buffer", 20, 80, 20, false, false},
		{"buffer disabled", 0, 80, 85, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			meter := api.NewMockMeter(ctrl)
			meter.EXPECT().CurrentPower().Return(0.0, nil).AnyTimes()
			battery := api.NewMockBattery(ctrl)
			battery.EXPECT().Soc().Return(tc.soc, nil).AnyTimes()
			var bat api.Meter = &struct {
				api.Meter
				api.Battery
			}{
				Meter:   meter,
				Battery: battery,
			}

			site := &Site{
				log:            util.NewLogger("test"),
				batteryMeters:  []config.Device[api.Meter]{config.NewStaticDevice(config.Named{}, bat)},
				bufferSoc:      tc.bufferSoc,
				bufferStartSoc: tc.startSoc,
			}

			state, err := site.updateMeters()
			require.NoError(t, err)
			res := site.sitePower(state, 0, 0)
			assert.Equal(t, tc.wantBuffered, res.batteryBuffered)
			assert.Equal(t, tc.wantStart, res.batteryStart)
		})
	}
}

func TestSitePowerContinuousBatteryPriorityAdjustment(t *testing.T) {
	t.Run("boost reconstruction", func(t *testing.T) {
		site, _ := continuousPriorityTestSite(t, -100, 29)

		res := continuousPrioritySitePower(site, 0)
		assert.Equal(t, 5100.0, res.power)
		assert.Equal(t, -5000.0, res.priorityAdjustment)
		assert.Equal(t, 100.0, res.power+res.priorityAdjustment, "battery boost reconstructs unadjusted site power")
	})

	t.Run("loadpoint flexibility", func(t *testing.T) {
		site, _ := continuousPriorityTestSite(t, -100, 29)

		res := continuousPrioritySitePower(site, 1000)
		assert.Equal(t, 4100.0, res.power, "loadpoint flexibility is applied once")
		assert.Equal(t, -900.0, res.power+res.priorityAdjustment, "reservation remains reversible after prioritization")

		next := continuousPrioritySitePower(site, 1000)
		assert.Equal(t, res.power, next.power, "multiple loadpoint updates do not accumulate reservation")
		assert.Equal(t, res.priorityAdjustment, next.priorityAdjustment)
	})

	t.Run("surplus above capability", func(t *testing.T) {
		site, _ := continuousPriorityTestSite(t, -100, 29)
		site.auxPower = 7000

		res := continuousPrioritySitePower(site, 0)
		assert.Equal(t, -1900.0, res.power, "surplus beyond charge capability remains available")
	})

	t.Run("excess dc", func(t *testing.T) {
		site, _ := continuousPriorityTestSite(t, -100, 29)
		site.excessDCPower = 500

		res := continuousPrioritySitePower(site, 0)
		assert.Equal(t, 5600.0, res.power)
		assert.Equal(t, -5000.0, res.priorityAdjustment, "known charge limit remains an AC reservation")
		assert.Equal(t, 600.0, res.power+res.priorityAdjustment)
	})

	t.Run("observed saturation with excess dc", func(t *testing.T) {
		site, _ := continuousPriorityTestSite(t, -1500, 29)
		site.batteryPowerRegulator.appliedCommand = -3000
		site.excessDCPower = 500

		res := continuousPrioritySitePower(site, 0)
		assert.Equal(t, 200.0, res.power)
		assert.Equal(t, -1000.0, res.priorityAdjustment, "only observed AC charging remains reserved")
		assert.Equal(t, -800.0, res.power+res.priorityAdjustment)
	})
}

func TestSitePowerContinuousBatteryPriorityStartupAndStaleSoc(t *testing.T) {
	t.Run("zero power startup", func(t *testing.T) {
		site, _ := continuousPriorityTestSite(t, 0, 29)
		resetContinuousPriorityController(site)

		res := continuousPrioritySitePower(site, 0)
		assert.Equal(t, 5200.0, res.power)
		assert.Equal(t, -5000.0, res.priorityAdjustment)
		assert.True(t, site.batteryPowerRegulator.policy.updatedAt.IsZero(), "startup reservation must precede stored policy")
	})

	t.Run("priority soc reached", func(t *testing.T) {
		site, _ := continuousPriorityTestSite(t, -100, 50)

		res := continuousPrioritySitePower(site, 0)
		assert.Equal(t, 100.0, res.power)
		assert.Zero(t, res.priorityAdjustment)
	})

	t.Run("zero residual remains reversible", func(t *testing.T) {
		site, _ := continuousPriorityTestSite(t, -100, 29)
		site.ResidualPower = 0
		site.batteryPowerRegulator.policy.residualPower = 0

		res := continuousPrioritySitePower(site, 0)
		assert.Equal(t, 5000.0, res.power)
		assert.Equal(t, -5100.0, res.priorityAdjustment)
		assert.Equal(t, -100.0, res.power+res.priorityAdjustment)
	})
}

// TestSitePowerContinuousBatteryPriorityFallback proves that an unavailable
// continuous reservation falls back to upstream measured-power priority.
func TestSitePowerContinuousBatteryPriorityFallback(t *testing.T) {
	plain := func(b *priorityTestBattery) api.Meter { return b }
	withModes := func(b *priorityTestBattery) api.Meter { return priorityTestModeBattery{b} }

	for _, tc := range []struct {
		name   string
		meter  func(*priorityTestBattery) api.Meter
		mutate func(*Site)
		update bool
	}{
		{"stale soc", plain, func(site *Site) {
			battery := site.batteryMeters[0].Instance().(*priorityTestBattery)
			battery.socErr = errors.New("soc unavailable")
			site.batterySocUpdated = []time.Time{time.Now().Add(-batteryPowerSocCacheMaxAge - time.Second)}
		}, true},
		{"controller fault", plain, func(site *Site) {
			site.batteryPowerRegulator.phase = batteryPowerFaultStopping
		}, false},
		{"controller released", plain, func(site *Site) {
			site.batteryPowerRegulator.phase = batteryPowerReleased
		}, false},
		{"charging cooldown", plain, func(site *Site) {
			site.batteryPowerRegulator.chargeBlockedUntil = site.batteryPowerRegulator.clock.Now().Add(time.Minute)
		}, false},
		{"discrete mode", withModes, func(site *Site) {
			site.batteryMode = api.BatteryHold
		}, false},
		{"mode handoff failed", plain, func(site *Site) {
			site.batteryModeHandoffFailed = true
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site, _ := continuousPriorityTestSiteWith(t, -100, 29, tc.meter)
			tc.mutate(site)

			state := site.state()
			if tc.update {
				var err error
				state, err = site.updateMeters()
				require.NoError(t, err)
			}
			res := site.sitePower(state, 0, 0)

			// upstream: charging power is hidden from the loadpoints
			assert.Equal(t, 200.0, res.power)
			assert.Equal(t, -100.0, res.priorityAdjustment)
			assert.Equal(t, 100.0, res.power+res.priorityAdjustment)
		})
	}
}

// TestSitePowerContinuousBatteryPriorityMixedSite proves that uncontrolled
// batteries keep measured priority for their own charging power while the
// controlled battery is covered by its reservation only.
func TestSitePowerContinuousBatteryPriorityMixedSite(t *testing.T) {
	for _, tc := range []struct {
		name              string
		aggregateSoc      float64
		uncontrolledPower float64
		expPower          float64
		expAdjustment     float64
	}{
		{"uncontrolled charging", 29, -800, 5100, -5800},
		{"uncontrolled discharging", 29, 800, 5900, -5000},
		{"aggregate soc at priority", 50, -800, 4300, -5000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site, _ := continuousPriorityTestSite(t, -100, 29)

			uncontrolled := &priorityTestBattery{power: tc.uncontrolledPower, soc: 40}
			controlled := site.batteryMeters[0]
			site.batteryMeters = []config.Device[api.Meter]{
				config.NewStaticDevice(config.Named{Name: "uncontrolled"}, api.Meter(uncontrolled)),
				controlled,
			}
			site.batteryPowerRegulator.battery.siteIndex = 1
			site.batterySocUpdated = append([]time.Time{time.Now()}, site.batterySocUpdated...)

			uncontrolledSoc, controlledSoc := 40.0, 29.0
			site.battery = types.BatteryState{
				Power: tc.uncontrolledPower - 100,
				Soc:   tc.aggregateSoc,
				Devices: []types.Measurement{
					{Power: tc.uncontrolledPower, Soc: &uncontrolledSoc},
					{Power: -100, Soc: &controlledSoc},
				},
			}

			res := continuousPrioritySitePower(site, 0)
			assert.Equal(t, tc.expPower, res.power)
			assert.Equal(t, tc.expAdjustment, res.priorityAdjustment)
			assert.Equal(t, tc.uncontrolledPower-100+200, res.power+res.priorityAdjustment,
				"battery boost reconstructs measured site power")
		})
	}
}

func TestContinuousBatteryPriorityStartupStartsPVDisablePath(t *testing.T) {
	site, clck := continuousPriorityTestSite(t, 0, 29)
	resetContinuousPriorityController(site)

	sitePower := continuousPrioritySitePower(site, 0).power

	previousVoltage := Voltage
	Voltage = 230
	t.Cleanup(func() {
		Voltage = previousVoltage
	})

	const disableDelay = 30 * time.Second
	lp := &Loadpoint{
		log:            util.NewLogger(t.Name()),
		clock:          clck,
		minCurrent:     6,
		maxCurrent:     16,
		phases:         3,
		measuredPhases: 3,
		offeredCurrent: 12,
		status:         api.StatusC,
		enabled:        true,
		Disable: loadpoint.ThresholdConfig{
			Threshold: 500,
			Delay:     disableDelay,
		},
	}

	assert.Equal(t, 6.0, lp.pvMaxCurrent(sitePower, 0, false, false))
	assert.False(t, lp.pvTimer.IsZero(), "first site cycle starts the disable timer")

	clck.Add(disableDelay)
	sitePower = continuousPrioritySitePower(site, 0).power
	assert.Zero(t, lp.pvMaxCurrent(sitePower, 0, false, false), "second site cycle disables charging")
}

func TestContinuousBatteryPrioritySurvivesDischargeCorrection(t *testing.T) {
	site, clck := continuousPriorityTestSite(t, 0, 29)
	resetContinuousPriorityController(site)
	battery := site.batteryMeters[0].Instance().(*priorityTestBattery)

	chargeSnapshot := batteryChargeControlSnapshot{}
	state, err := site.updateMeters()
	require.NoError(t, err)
	first := site.sitePowerWithBatteryChargeSnapshot(state, 0, 0, &chargeSnapshot)
	assert.Equal(t, -5000.0, first.priorityAdjustment)
	// updateMeters reads limits for the discharge cap; the charge snapshot reads them again.
	assert.Equal(t, 2, battery.limitReads)
	assert.Equal(t, 2, battery.socReads)

	site.updateBatteryPowerControlPolicyWithSnapshot(api.Rate{}, true, chargeSnapshot)
	require.Equal(t, batteryPowerNeutral, site.batteryPowerRegulator.phase)
	assert.Equal(t, 2, battery.limitReads, "loadpoint and regulator decisions share one limit snapshot")
	assert.Equal(t, 2, battery.socReads)

	grid := site.batteryPowerRegulator.gridMeter.(*regulatorTestMeter)
	grid.set(3000, nil)
	clck.Add(batteryPowerControlInterval)
	site.batteryPowerRegulator.tick()
	require.Equal(t, batteryPowerDischarging, site.batteryPowerRegulator.phase)
	battery.power = site.batteryPowerRegulator.appliedCommand
	require.Positive(t, battery.power)

	state, err = site.updateMeters()
	require.NoError(t, err)
	res := site.sitePower(state, 0, 0)
	assert.Equal(t, 5200+battery.power, res.power)
	assert.Equal(t, -5000.0, res.priorityAdjustment, "EV-induced discharge must not release battery priority")
	assert.Equal(t, battery.power+200, res.power+res.priorityAdjustment, "battery boost reconstructs measured site power")
}

func TestGreenShare(t *testing.T) {
	tc := []struct {
		title                                                 string
		grid, pv, battery, home, lp                           float64
		greenShareTotal, greenShareHome, greenShareLoadpoints float64
	}{
		{
			"half grid, half pv, green home",
			1000, 1000, 0, 1000, 1000,
			0.5, 1, 0,
		},
		{
			"half grid, half pv, no home",
			1000, 1000, 0, 0, 2000,
			0.5, 1, 0.5,
		},
		{
			"half grid, half pv, no lp",
			2500, 2500, 0, 5000, 0,
			0.5, 0.5, 0,
		},
		{
			"full pv",
			0, 5000, 0, 1000, 4000,
			1, 1, 1,
		},
		{
			"full grid",
			5000, 0, 0, 1000, 4000,
			0, 0, 0,
		},
		{
			"half grid, half battery, green home",
			1000, 0, 1000, 1000, 1000,
			0.5, 1, 0,
		},
		{
			"half grid, half battery, no home",
			1000, 0, 1000, 0, 2000,
			0.5, 1, 0.5,
		},
		{
			"half grid, half battery, no lp",
			1000, 0, 1000, 2000, 0,
			0.5, 0.5, 0,
		},
		{
			"full pv, pv export",
			-5000, 10000, 0, 1000, 4000,
			1, 1, 1,
		},
		{
			"full pv, pv export, no lp",
			-5000, 10000, 0, 5000, 0,
			1, 1, 1,
		},
		{
			"full pv, pv export, battery charge",
			-2500, 10000, -2500, 1000, 4000,
			1, 1, 1,
		},
		{
			"full grid, battery charge",
			3000, 0, -1000, 1000, 1000,
			0, 0, 0,
		},
		{
			"full grid, battery charge, no lp",
			2000, 0, -1000, 1000, 0,
			0, 0, 0,
		},
		{
			"half grid, half pv, battery charge, no lp",
			1000, 1000, -1000, 1000, 0,
			0.5, 1, 0,
		},
		{
			"half grid, half pv, battery charge, home, lp",
			1000, 1000, -1000, 500, 500,
			0.5, 1, 0,
		},
		{
			"pv ac limited, battery charge & grid import",
			1000, 3000, -1000, 1000, 2000,
			0.75, 1, 0.5,
		},
	}

	for _, tc := range tc {
		t.Log(tc.title)

		s := &Site{
			siteState: siteState{
				gridPower: tc.grid,
				pvPower:   tc.pv,
				battery: types.BatteryState{
					Power: tc.battery,
				},
			},
		}

		totalPower := tc.grid + tc.pv + max(0, tc.battery)
		greenShareTotal := s.greenShare(0, totalPower)
		if greenShareTotal != tc.greenShareTotal {
			t.Errorf("greenShareTotal wanted %.3f, got %.3f", tc.greenShareTotal, greenShareTotal)
		}
		greenShareHome := s.greenShare(0, tc.home)
		if greenShareHome != tc.greenShareHome {
			t.Errorf("greenShareHome wanted %.3f, got %.3f", tc.greenShareHome, greenShareHome)
		}
		greenShareLoadpoints := s.greenShare(tc.home+max(0, -tc.battery), totalPower)
		if greenShareLoadpoints != tc.greenShareLoadpoints {
			t.Errorf("greenShareLoadpoints wanted %.3f, got %.3f", tc.greenShareLoadpoints, greenShareLoadpoints)
		}
	}
}

func TestRequiredBatteryMode(t *testing.T) {
	tc := []struct {
		gridChargeActive    bool
		gridDischargeActive bool
		mode, res           api.BatteryMode
	}{
		{false, false, api.BatteryUnknown, api.BatteryUnknown}, // ignore
		{false, false, api.BatteryNormal, api.BatteryUnknown},  // ignore
		{false, false, api.BatteryHold, api.BatteryNormal},
		{false, false, api.BatteryCharge, api.BatteryNormal},

		{true, false, api.BatteryUnknown, api.BatteryCharge},
		{true, false, api.BatteryNormal, api.BatteryCharge},
		{true, false, api.BatteryHold, api.BatteryCharge},
		{true, false, api.BatteryCharge, api.BatteryUnknown}, // ignore

		// grid discharge (feed-in arbitrage)
		{false, true, api.BatteryUnknown, api.BatteryDischarge},
		{false, true, api.BatteryNormal, api.BatteryDischarge},
		{false, true, api.BatteryHold, api.BatteryDischarge},
		{false, true, api.BatteryDischarge, api.BatteryUnknown}, // ignore
		{false, true, api.BatteryCharge, api.BatteryDischarge},

		// grid charge wins over grid discharge when both active
		{true, true, api.BatteryNormal, api.BatteryCharge},
	}

	{
		// no battery
		res := new(Site).requiredBatteryMode(true, false, api.Rate{})
		assert.Equal(t, api.BatteryUnknown, res, "expected %s, got %s", api.BatteryUnknown, res)
	}

	for _, tc := range tc {
		t.Logf("%+v", tc)

		s := &Site{
			log:           util.NewLogger("foo"),
			batteryMeters: []config.Device[api.Meter]{nil},
			batteryMode:   tc.mode,
		}

		res := s.requiredBatteryMode(tc.gridChargeActive, tc.gridDischargeActive, api.Rate{})
		assert.Equal(t, tc.res, res, "expected %s, got %s", tc.res, res)
	}
}
