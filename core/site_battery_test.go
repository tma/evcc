package core

import (
	"errors"
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/types"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// TestBatterySocRetainOnReadError guards that a failed soc read keeps the last
// known soc instead of reporting the pack as empty (discussion #26560).
func TestBatterySocRetainOnReadError(t *testing.T) {
	ctrl := gomock.NewController(t)

	meter := api.NewMockMeter(ctrl)
	meter.EXPECT().CurrentPower().Return(0.0, nil).AnyTimes()

	battery := api.NewMockBattery(ctrl)
	battery.EXPECT().Soc().Return(0.0, errors.New("read failed")).AnyTimes()

	var bat api.Meter = &struct {
		api.Meter
		api.Battery
	}{
		Meter:   meter,
		Battery: battery,
	}

	site := &Site{
		log:           util.NewLogger("foo"),
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{Name: "bat"}, bat)},
	}
	site.battery.Soc = 84
	soc := 84.0
	site.battery.Devices = []types.Measurement{{Soc: &soc}}
	site.batterySocUpdated = []time.Time{time.Now()}

	site.updateBatteryMeters()

	assert.Equal(t, 84.0, site.battery.Soc, "soc retained when the read fails")
	assert.Equal(t, 84.0, *site.battery.Devices[0].Soc, "recent per-device soc retained when the read fails")
}

func TestApplyBatteryMode(t *testing.T) {
	for _, tc := range []struct {
		internal, expected api.BatteryMode
	}{
		{api.BatteryUnknown, api.BatteryUnknown}, // no change required
		{api.BatteryNormal, api.BatteryUnknown},  // no change required
		{api.BatteryHold, api.BatteryNormal},
		{api.BatteryCharge, api.BatteryNormal},
	} {
		t.Logf("%+v", tc)

		ctrl := gomock.NewController(t)

		var bat api.Meter
		batCon := api.NewMockBatteryController(ctrl)

		bat = &struct {
			api.Meter
			api.BatteryController
		}{
			BatteryController: batCon,
		}

		site := &Site{
			log:           util.NewLogger("foo"),
			batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{}, bat)},
			batteryMode:   tc.internal,
		}

		// verify mode applied to battery
		if tc.expected != api.BatteryUnknown {
			batCon.EXPECT().SetBatteryMode(tc.expected).Times(1)
		}
		site.updateBatteryMode(false, api.Rate{})

		if tc.internal != api.BatteryNormal {
			assert.Equal(t, tc.expected, site.batteryMode)
		}

		ctrl.Finish()
	}
}

func TestDischargeControlHoldAtBufferSoc(t *testing.T) {
	lp := &Loadpoint{mode: api.ModeNow}
	lp.setStatus(api.StatusC)

	site := &Site{
		log:               util.NewLogger("test"),
		loadpoints:        []*Loadpoint{lp},
		batteryMeters:     []config.Device[api.Meter]{nil},
		batterySocUpdated: []time.Time{time.Now()},
		bufferSoc:         50,
	}

	site.battery.Soc = 80
	assert.False(t, site.dischargeControlActive(api.Rate{}))

	site.battery.Soc = 50
	assert.True(t, site.dischargeControlActive(api.Rate{}))

	site.battery.Soc = 51
	assert.True(t, site.dischargeControlActive(api.Rate{}), "hold remains active after the battery rebounds")

	require.NoError(t, site.SetBufferSoc(40))
	assert.False(t, site.dischargeControlActive(api.Rate{}), "changing buffer soc clears the hold latch")

	lp.setStatus(api.StatusB)
	site.batteryDischargeHold = true
	assert.False(t, site.dischargeControlActive(api.Rate{}))
	assert.False(t, site.batteryDischargeHold)

	lp.setStatus(api.StatusC)
	site.battery.Soc = 80
	assert.False(t, site.dischargeControlActive(api.Rate{}), "a new fast charge may use the battery above the buffer")
}

func TestRequiredBatteryModeUsesDischargeHold(t *testing.T) {
	lp := &Loadpoint{mode: api.ModeNow}
	lp.setStatus(api.StatusC)

	site := &Site{
		log:               util.NewLogger("test"),
		loadpoints:        []*Loadpoint{lp},
		batteryMeters:     []config.Device[api.Meter]{nil},
		batterySocUpdated: []time.Time{time.Now()},
		batteryMode:       api.BatteryNormal,
		bufferSoc:         50,
	}
	site.battery.Soc = 50

	assert.Equal(t, api.BatteryHold, site.requiredBatteryMode(false, api.Rate{}))
}

func TestDischargeControlBufferBoundaries(t *testing.T) {
	lp := &Loadpoint{mode: api.ModeNow}
	lp.setStatus(api.StatusC)

	for _, bufferSoc := range []float64{0, 100} {
		site := &Site{
			log:               util.NewLogger("test"),
			loadpoints:        []*Loadpoint{lp},
			batteryMeters:     []config.Device[api.Meter]{nil},
			batterySocUpdated: []time.Time{time.Now()},
			bufferSoc:         bufferSoc,
		}
		site.battery.Soc = 50

		assert.False(t, site.dischargeControlActive(api.Rate{}))
	}
}

func TestDischargeControlPreventHoldsImmediately(t *testing.T) {
	lp := &Loadpoint{mode: api.ModeNow}
	lp.setStatus(api.StatusC)

	site := &Site{
		log:                     util.NewLogger("test"),
		loadpoints:              []*Loadpoint{lp},
		batteryMeters:           []config.Device[api.Meter]{nil},
		batterySocUpdated:       []time.Time{time.Now()},
		bufferSoc:               50,
		batteryDischargeControl: true,
	}
	site.battery.Soc = 80

	assert.True(t, site.dischargeControlActive(api.Rate{}))
	assert.False(t, site.batteryDischargeHold)
}

func TestDischargeControlReleasesHoldWhileGridCharging(t *testing.T) {
	lp := &Loadpoint{mode: api.ModeNow}
	lp.setStatus(api.StatusB)

	site := &Site{
		log:                  util.NewLogger("test"),
		loadpoints:           []*Loadpoint{lp},
		batteryMeters:        []config.Device[api.Meter]{nil},
		batteryMode:          api.BatteryHold,
		bufferSoc:            50,
		batteryDischargeHold: true,
	}

	assert.Equal(t, api.BatteryCharge, site.requiredBatteryMode(true, api.Rate{}))
	assert.False(t, site.batteryDischargeHold)
}

func TestDischargeControlWaitsForBatterySoc(t *testing.T) {
	lp := &Loadpoint{mode: api.ModeNow}
	lp.setStatus(api.StatusC)

	site := &Site{
		log:               util.NewLogger("test"),
		loadpoints:        []*Loadpoint{lp},
		batteryMeters:     []config.Device[api.Meter]{nil},
		batterySocUpdated: []time.Time{{}},
		bufferSoc:         50,
	}
	site.battery.Soc = 80

	assert.True(t, site.dischargeControlActive(api.Rate{}))
	assert.False(t, site.batteryDischargeHold, "missing soc only holds discharge until the next evaluation")

	site.batterySocUpdated[0] = time.Now()
	assert.False(t, site.dischargeControlActive(api.Rate{}))
}

func TestApplyBatteryModeStopsPowerControlFirst(t *testing.T) {
	ctrl := gomock.NewController(t)
	powerController := api.NewMockBatteryPowerController(ctrl)
	modeController := api.NewMockBatteryController(ctrl)

	var bat api.Meter = &struct {
		api.Meter
		api.BatteryController
		api.BatteryPowerController
	}{
		BatteryController:      modeController,
		BatteryPowerController: powerController,
	}

	site := &Site{
		log:           util.NewLogger("foo"),
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{}, bat)},
	}

	gomock.InOrder(
		powerController.EXPECT().SetBatteryPower(0.0),
		modeController.EXPECT().SetBatteryMode(api.BatteryHold),
	)

	assert.NoError(t, site.applyBatteryMode(api.BatteryHold))
}

func TestApplyBatteryModeRequiresPowerRelease(t *testing.T) {
	ctrl := gomock.NewController(t)
	powerController := api.NewMockBatteryPowerController(ctrl)
	modeController := api.NewMockBatteryController(ctrl)

	var bat api.Meter = &struct {
		api.Meter
		api.BatteryController
		api.BatteryPowerController
	}{
		BatteryController:      modeController,
		BatteryPowerController: powerController,
	}
	site := &Site{
		log:           util.NewLogger("foo"),
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{}, bat)},
	}

	powerController.EXPECT().SetBatteryPower(0.0).Return(api.ErrNotAvailable)

	assert.ErrorIs(t, site.applyBatteryMode(api.BatteryHold), api.ErrNotAvailable)
}

func TestApplyBatteryModeSkipsUnavailableMode(t *testing.T) {
	ctrl := gomock.NewController(t)
	first := api.NewMockBatteryController(ctrl)
	second := api.NewMockBatteryController(ctrl)

	var firstBattery api.Meter = &struct {
		api.Meter
		api.BatteryController
	}{BatteryController: first}
	var secondBattery api.Meter = &struct {
		api.Meter
		api.BatteryController
	}{BatteryController: second}
	site := &Site{
		log: util.NewLogger("foo"),
		batteryMeters: []config.Device[api.Meter]{
			config.NewStaticDevice(config.Named{Name: "first"}, firstBattery),
			config.NewStaticDevice(config.Named{Name: "second"}, secondBattery),
		},
	}

	first.EXPECT().SetBatteryMode(api.BatteryCharge).Return(api.ErrNotAvailable)
	second.EXPECT().SetBatteryMode(api.BatteryCharge)

	assert.NoError(t, site.applyBatteryMode(api.BatteryCharge))
}

func TestApplyBatteryModeReleasesRegulatorFirst(t *testing.T) {
	ctrl := gomock.NewController(t)
	powerController := api.NewMockBatteryPowerController(ctrl)
	modeController := api.NewMockBatteryController(ctrl)

	var bat api.Meter = &struct {
		api.Meter
		api.BatteryController
		api.BatteryPowerController
	}{
		Meter:                  &regulatorTestMeter{},
		BatteryController:      modeController,
		BatteryPowerController: powerController,
	}
	devices := []config.Device[api.Meter]{
		config.NewStaticDevice(config.Named{Name: "battery"}, bat),
	}
	site := &Site{
		log:           util.NewLogger("foo"),
		gridMeter:     config.NewStaticDevice[api.Meter](config.Named{Name: "grid"}, &regulatorTestMeter{}),
		batteryMeters: devices,
	}
	site.batteryPowerRegulator = newBatteryPowerRegulator(site.log, site.gridMeter.Instance(), devices)

	gomock.InOrder(
		powerController.EXPECT().SetBatteryPower(0.0),
		modeController.EXPECT().SetBatteryMode(api.BatteryHold),
	)

	assert.NoError(t, site.applyBatteryMode(api.BatteryHold))
}

func TestApplyBatteryModeWaitsForBackedOffRegulatorStop(t *testing.T) {
	ctrl := gomock.NewController(t)
	modeController := api.NewMockBatteryController(ctrl)
	powerController := &regulatorTestController{}

	var bat api.Meter = &struct {
		api.Meter
		api.BatteryController
		api.BatteryPowerController
		api.BatteryPowerLimiter
	}{
		Meter:                  &regulatorTestMeter{},
		BatteryController:      modeController,
		BatteryPowerController: powerController,
		BatteryPowerLimiter:    testBatteryPowerLimiter{charge: 5000, discharge: 5000},
	}
	devices := []config.Device[api.Meter]{
		config.NewStaticDevice(config.Named{Name: "battery"}, bat),
	}
	site := &Site{
		log:           util.NewLogger("foo"),
		gridMeter:     config.NewStaticDevice[api.Meter](config.Named{Name: "grid"}, &regulatorTestMeter{}),
		batteryMeters: devices,
	}
	regulator := newBatteryPowerRegulator(site.log, site.gridMeter.Instance(), devices)
	clck := clock.NewMock()
	regulator.clock = clck
	regulator.phase = batteryPowerFaultStopping
	regulator.appliedCommand = -1500
	regulator.initialized = true
	regulator.stopFailureSince = clck.Now().Add(-batteryPowerStopRetrySafetyWindow)
	regulator.lastStopAttemptAt = clck.Now()
	site.batteryPowerRegulator = regulator

	assert.ErrorIs(t, site.applyBatteryMode(api.BatteryHold), errBatteryPowerStopRetryPending)
	assert.Empty(t, powerController.values())
}

func TestFailedBatteryModeHandoffKeepsRegulatorReleased(t *testing.T) {
	ctrl := gomock.NewController(t)
	powerController := api.NewMockBatteryPowerController(ctrl)
	modeController := api.NewMockBatteryController(ctrl)
	modeErr := errors.New("mode failed")

	var bat api.Meter = &struct {
		api.Meter
		api.BatteryController
		api.BatteryPowerController
		api.BatteryPowerLimiter
	}{
		BatteryController:      modeController,
		BatteryPowerController: powerController,
		BatteryPowerLimiter:    testBatteryPowerLimiter{charge: 5000, discharge: 5000},
	}
	devices := []config.Device[api.Meter]{
		config.NewStaticDevice(config.Named{Name: "battery"}, bat),
	}
	site := &Site{
		log:           util.NewLogger("foo"),
		gridMeter:     config.NewStaticDevice[api.Meter](config.Named{Name: "grid"}, &regulatorTestMeter{}),
		batteryMeters: devices,
		batteryMode:   api.BatteryNormal,
	}
	site.batteryPowerRegulator = newBatteryPowerRegulator(site.log, site.gridMeter.Instance(), devices)

	gomock.InOrder(
		powerController.EXPECT().SetBatteryPower(0.0),
		modeController.EXPECT().SetBatteryMode(api.BatteryCharge).Return(modeErr),
	)

	modeReady := site.updateBatteryMode(true, api.Rate{})
	assert.False(t, modeReady)
	site.updateBatteryPowerControlPolicy(api.Rate{}, modeReady)
	assert.Equal(t, batteryPowerReleased, site.batteryPowerRegulator.phase)

	modeController.EXPECT().SetBatteryMode(api.BatteryNormal)
	modeReady = site.updateBatteryMode(false, api.Rate{})
	assert.True(t, modeReady)
	assert.False(t, site.batteryModeHandoffFailed)
}

func TestRequiredExternalBatteryMode(t *testing.T) {
	for _, tc := range []struct {
		internal, external, new api.BatteryMode
	}{
		{api.BatteryUnknown, api.BatteryUnknown, api.BatteryUnknown},
		{api.BatteryUnknown, api.BatteryNormal, api.BatteryNormal},
		{api.BatteryUnknown, api.BatteryCharge, api.BatteryCharge},

		{api.BatteryNormal, api.BatteryUnknown, api.BatteryUnknown},
		{api.BatteryNormal, api.BatteryNormal, api.BatteryUnknown}, // no change required
		{api.BatteryNormal, api.BatteryCharge, api.BatteryCharge},

		{api.BatteryCharge, api.BatteryUnknown, api.BatteryNormal},
		{api.BatteryCharge, api.BatteryNormal, api.BatteryNormal},
		{api.BatteryCharge, api.BatteryCharge, api.BatteryUnknown}, // no change required
	} {
		t.Logf("%+v", tc)

		site := &Site{
			log:           util.NewLogger("foo"),
			batteryMeters: []config.Device[api.Meter]{nil},
		}

		site.batteryMode = tc.internal
		site.batteryModeExternal = tc.external

		mode := site.requiredBatteryMode(false, api.Rate{})
		assert.Equal(t, tc.new.String(), mode.String(), "internal mode expected %s got %s", tc.new, mode)
	}
}

func TestExternalBatteryModeChange(t *testing.T) {
	for _, tc := range []struct {
		internal, external, expected api.BatteryMode
	}{
		{api.BatteryUnknown, api.BatteryUnknown, api.BatteryUnknown},
		{api.BatteryUnknown, api.BatteryNormal, api.BatteryNormal},
		{api.BatteryUnknown, api.BatteryCharge, api.BatteryCharge},

		{api.BatteryNormal, api.BatteryUnknown, api.BatteryUnknown},
		{api.BatteryNormal, api.BatteryNormal, api.BatteryUnknown},
		{api.BatteryNormal, api.BatteryCharge, api.BatteryCharge},

		{api.BatteryHold, api.BatteryUnknown, api.BatteryNormal}, // return to normal
		{api.BatteryHold, api.BatteryNormal, api.BatteryNormal},
		{api.BatteryHold, api.BatteryHold, api.BatteryUnknown},

		{api.BatteryCharge, api.BatteryUnknown, api.BatteryNormal}, // return to normal
		{api.BatteryCharge, api.BatteryNormal, api.BatteryNormal},
		{api.BatteryCharge, api.BatteryCharge, api.BatteryUnknown},
	} {
		t.Logf("%+v", tc)

		ctrl := gomock.NewController(t)

		var bat api.Meter
		batCon := api.NewMockBatteryController(ctrl)

		bat = &struct {
			api.Meter
			api.BatteryController
		}{
			BatteryController: batCon,
		}

		site := &Site{
			log:           util.NewLogger("foo"),
			batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{}, bat)},
			batteryMode:   tc.internal,
		}

		// 1. set required external mode
		site.SetBatteryModeExternal(tc.external)
		assert.Equal(t, site.batteryModeExternal, tc.external, "external mode expected %s got %s", tc.external, site.batteryModeExternal)
		assert.Equal(t, site.batteryMode, tc.internal, "internal mode expected unchanged %s got %s", tc.internal, site.batteryMode)

		// 2. verify external mode applied to battery
		if tc.expected != api.BatteryUnknown {
			batCon.EXPECT().SetBatteryMode(tc.expected).Times(1)
		}
		site.updateBatteryMode(false, api.Rate{})
		if !ctrl.Satisfied() {
			ctrl.Finish()
		}

		// 3. verify required external mode only applied once
		site.updateBatteryMode(false, api.Rate{})
		if !ctrl.Satisfied() {
			ctrl.Finish()
		}

		// 4. verify timer expiry
		site.batteryModeExternalTimer = site.batteryModeExternalTimer.Add(-time.Hour)
		site.batteryModeWatchdogExpired()

		// mode reverted to unknown, timer still active
		assert.Equal(t, site.batteryModeExternal, api.BatteryUnknown)
		assert.False(t, site.batteryModeExternalTimer.IsZero())

		// battery switched back to normal mode
		batCon.EXPECT().SetBatteryMode(api.BatteryNormal).Times(1)
		site.updateBatteryMode(false, api.Rate{})

		// timer disabled
		assert.True(t, site.batteryModeExternalTimer.IsZero())

		ctrl.Finish()
	}
}

func TestForcedBatteryChargeLimits(t *testing.T) {
	limit := 80.0

	for _, tc := range []struct {
		internal, expected api.BatteryMode
		soc                float64
	}{
		{api.BatteryUnknown, api.BatteryCharge, 50},
		{api.BatteryUnknown, api.BatteryHold, 90},

		{api.BatteryNormal, api.BatteryCharge, 50},
		{api.BatteryNormal, api.BatteryHold, 90},

		{api.BatteryHold, api.BatteryCharge, 50},
		{api.BatteryHold, api.BatteryHold, 90}, // TODO make this api.BatteryUnknown

		{api.BatteryCharge, api.BatteryUnknown, 50},
		{api.BatteryCharge, api.BatteryHold, 90},
	} {
		t.Logf("%+v", tc)

		ctrl := gomock.NewController(t)

		var bat api.Meter
		batSoc := api.NewMockBattery(ctrl)
		batCon := api.NewMockBatteryController(ctrl)
		batSocLimit := api.NewMockBatterySocLimiter(ctrl)

		bat = &struct {
			api.Meter
			api.Battery
			api.BatteryController
			api.BatterySocLimiter
		}{
			Meter:             bat,
			Battery:           batSoc,
			BatteryController: batCon,
			BatterySocLimiter: batSocLimit,
		}

		site := &Site{
			log:           util.NewLogger("foo"),
			batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{}, bat)},
			batteryMode:   tc.internal,
		}

		batSoc.EXPECT().Soc().Return(tc.soc, nil).Times(1)
		batSocLimit.EXPECT().GetSocLimits().Return(0.0, limit).Times(1)

		if tc.expected != api.BatteryUnknown {
			batCon.EXPECT().SetBatteryMode(tc.expected).Times(1)
		}

		site.updateBatteryMode(true, api.Rate{})

		ctrl.Finish()
	}
}
