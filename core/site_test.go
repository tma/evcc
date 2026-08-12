package core

import (
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

func continuousPriorityTestSite(t *testing.T, batteryPower, batterySoc float64) (*Site, *clock.Mock) {
	t.Helper()

	ctrl := gomock.NewController(t)
	meter := api.NewMockMeter(ctrl)
	meter.EXPECT().CurrentPower().Return(batteryPower, nil).AnyTimes()
	battery := api.NewMockBattery(ctrl)
	battery.EXPECT().Soc().Return(batterySoc, nil).AnyTimes()

	var bat api.Meter = &struct {
		api.Meter
		api.Battery
	}{
		Meter:   meter,
		Battery: battery,
	}

	clck := clock.NewMock()
	now := clck.Now()
	regulator := &batteryPowerRegulator{
		clock: clck,
		battery: regulatedBattery{
			siteIndex: 0,
		},
		policy: batteryPowerControlPolicy{
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
		},
		phase:          batteryPowerCharging,
		appliedCommand: batteryPower,
		initialized:    true,
		lastBatterySample: batteryPowerSample{
			Value:      batteryPower,
			StartedAt:  now.Add(-time.Millisecond),
			FinishedAt: now,
		},
		policyMaxAge: batteryPowerPolicyMaxAge,
	}

	return &Site{
		log:                   util.NewLogger(t.Name()),
		batteryMeters:         []config.Device[api.Meter]{config.NewStaticDevice(config.Named{}, bat)},
		pvMeters:              []config.Device[api.Meter]{},
		prioritySoc:           50,
		ResidualPower:         200,
		batteryPowerRegulator: regulator,
	}, clck
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
			site.excessDCPower = tc.excessDC

			sitePower, _, _, adjustment, err := site.sitePower(0, 0)
			assert.NoError(t, err)
			assert.Equal(t, tc.expSitePower, sitePower, "sitePower")
			assert.Equal(t, tc.expAdjustment, adjustment, "priority adjustment")
			assert.Equal(t, tc.expReconstructed, sitePower+adjustment, "reconstructed (unadjusted) site power")
		})
	}
}

func TestSitePowerBatteryStartIndependentOfSolarSupport(t *testing.T) {
	for _, tc := range []struct {
		name         string
		solar        bool
		soc          float64
		wantBuffered bool
		wantStart    bool
	}{
		{"start without solar support", false, 85, false, true},
		{"below start without solar support", false, 70, false, false},
		{"start and buffer with solar support", true, 85, true, true},
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
				log:                 util.NewLogger("test"),
				batteryMeters:       []config.Device[api.Meter]{config.NewStaticDevice(config.Named{}, bat)},
				batterySolarSupport: tc.solar,
				batteryReserveSoc:   20,
				bufferStartSoc:      80,
			}

			_, buffered, start, _, err := site.sitePower(0, 0)
			assert.NoError(t, err)
			assert.Equal(t, tc.wantBuffered, buffered)
			assert.Equal(t, tc.wantStart, start)
		})
	}
}

func TestSitePowerContinuousBatteryPriorityAdjustment(t *testing.T) {
	t.Run("boost reconstruction", func(t *testing.T) {
		site, _ := continuousPriorityTestSite(t, -100, 29)

		sitePower, _, _, adjustment, err := site.sitePower(0, 0)
		require.NoError(t, err)
		assert.Equal(t, 5100.0, sitePower)
		assert.Equal(t, -5000.0, adjustment)
		assert.Equal(t, 100.0, sitePower+adjustment, "battery boost reconstructs unadjusted site power")
	})

	t.Run("loadpoint flexibility", func(t *testing.T) {
		site, _ := continuousPriorityTestSite(t, -100, 29)

		sitePower, _, _, adjustment, err := site.sitePower(0, 1000)
		require.NoError(t, err)
		assert.Equal(t, 4100.0, sitePower, "loadpoint flexibility is applied once")
		assert.Equal(t, -900.0, sitePower+adjustment, "reservation remains reversible after prioritization")

		nextSitePower, _, _, nextAdjustment, err := site.sitePower(0, 1000)
		require.NoError(t, err)
		assert.Equal(t, sitePower, nextSitePower, "multiple loadpoint updates do not accumulate reservation")
		assert.Equal(t, adjustment, nextAdjustment)
	})

	t.Run("surplus above capability", func(t *testing.T) {
		site, _ := continuousPriorityTestSite(t, -100, 29)
		site.auxPower = 7000

		sitePower, _, _, _, err := site.sitePower(0, 0)
		require.NoError(t, err)
		assert.Equal(t, -1900.0, sitePower, "surplus beyond charge capability remains available")
	})

	t.Run("excess dc", func(t *testing.T) {
		site, _ := continuousPriorityTestSite(t, -100, 29)
		site.excessDCPower = 500

		sitePower, _, _, adjustment, err := site.sitePower(0, 0)
		require.NoError(t, err)
		assert.Equal(t, 5600.0, sitePower)
		assert.Equal(t, -5000.0, adjustment, "known charge limit remains an AC reservation")
		assert.Equal(t, 600.0, sitePower+adjustment)
	})

	t.Run("observed saturation with excess dc", func(t *testing.T) {
		site, _ := continuousPriorityTestSite(t, -1500, 29)
		site.batteryPowerRegulator.appliedCommand = -3000
		site.excessDCPower = 500

		sitePower, _, _, adjustment, err := site.sitePower(0, 0)
		require.NoError(t, err)
		assert.Equal(t, 200.0, sitePower)
		assert.Equal(t, -1000.0, adjustment, "only observed AC charging remains reserved")
		assert.Equal(t, -800.0, sitePower+adjustment)
	})
}

func TestSitePowerContinuousBatteryPriorityStartupAndStaleSoc(t *testing.T) {
	t.Run("zero power startup", func(t *testing.T) {
		site, _ := continuousPriorityTestSite(t, 0, 29)
		site.batteryPowerRegulator.phase = batteryPowerNeutral
		site.batteryPowerRegulator.lastBatterySample = batteryPowerSample{}

		sitePower, _, _, adjustment, err := site.sitePower(0, 0)
		require.NoError(t, err)
		assert.Equal(t, 5200.0, sitePower)
		assert.Equal(t, -5000.0, adjustment)
	})

	t.Run("stale soc releases reservation", func(t *testing.T) {
		site, clck := continuousPriorityTestSite(t, -100, 29)
		site.batteryPowerRegulator.policy.socUpdatedAt = clck.Now().Add(-batteryPowerPolicyMaxAge - time.Second)

		sitePower, _, _, adjustment, err := site.sitePower(0, 0)
		require.NoError(t, err)
		assert.Equal(t, 100.0, sitePower)
		assert.Zero(t, adjustment)
	})

	t.Run("priority soc reached", func(t *testing.T) {
		site, _ := continuousPriorityTestSite(t, -100, 50)

		sitePower, _, _, adjustment, err := site.sitePower(0, 0)
		require.NoError(t, err)
		assert.Equal(t, 100.0, sitePower)
		assert.Zero(t, adjustment)
	})

	t.Run("zero residual remains reversible", func(t *testing.T) {
		site, _ := continuousPriorityTestSite(t, -100, 29)
		site.ResidualPower = 0
		site.batteryPowerRegulator.policy.residualPower = 0

		sitePower, _, _, adjustment, err := site.sitePower(0, 0)
		require.NoError(t, err)
		assert.Equal(t, 5000.0, sitePower)
		assert.Equal(t, -5100.0, adjustment)
		assert.Equal(t, -100.0, sitePower+adjustment)
	})
}

func TestContinuousBatteryPriorityStartsPVDisablePath(t *testing.T) {
	site, clck := continuousPriorityTestSite(t, -100, 29)
	sitePower, _, _, _, err := site.sitePower(0, 0)
	require.NoError(t, err)

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

	assert.Equal(t, 6.0, lp.pvMaxCurrent(api.ModePV, sitePower, 0, false, false))
	assert.False(t, lp.pvTimer.IsZero(), "first site cycle starts the disable timer")

	clck.Add(disableDelay)
	site.batteryPowerRegulator.lastBatterySample.StartedAt = clck.Now().Add(-time.Millisecond)
	site.batteryPowerRegulator.lastBatterySample.FinishedAt = clck.Now()
	sitePower, _, _, _, err = site.sitePower(0, 0)
	require.NoError(t, err)
	assert.Zero(t, lp.pvMaxCurrent(api.ModePV, sitePower, 0, false, false), "second site cycle disables charging")
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
			gridPower: tc.grid,
			pvPower:   tc.pv,
			battery: types.BatteryState{
				Power: tc.battery,
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
		gridChargeActive bool
		mode, res        api.BatteryMode
	}{
		{false, api.BatteryUnknown, api.BatteryUnknown}, // ignore
		{false, api.BatteryNormal, api.BatteryUnknown},  // ignore
		{false, api.BatteryHold, api.BatteryNormal},
		{false, api.BatteryCharge, api.BatteryNormal},

		{true, api.BatteryUnknown, api.BatteryCharge},
		{true, api.BatteryNormal, api.BatteryCharge},
		{true, api.BatteryHold, api.BatteryCharge},
		{true, api.BatteryCharge, api.BatteryUnknown}, // ignore
	}

	{
		// no battery
		res := new(Site).requiredBatteryMode(true, api.Rate{})
		assert.Equal(t, api.BatteryUnknown, res, "expected %s, got %s", api.BatteryUnknown, res)
	}

	for _, tc := range tc {
		t.Logf("%+v", tc)

		s := &Site{
			batteryMeters: []config.Device[api.Meter]{nil},
			batteryMode:   tc.mode,
		}

		res := s.requiredBatteryMode(tc.gridChargeActive, api.Rate{})
		assert.Equal(t, tc.res, res, "expected %s, got %s", tc.res, res)
	}
}
