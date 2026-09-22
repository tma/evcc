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

type testBatteryCapacity float64

func (c testBatteryCapacity) Capacity() float64 {
	return float64(c)
}

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
		api.BatteryCapacity
	}{
		Meter:           meter,
		Battery:         battery,
		BatteryCapacity: testBatteryCapacity(10),
	}

	site := &Site{
		log:           util.NewLogger("foo"),
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{Name: "bat"}, bat)},
	}
	site.battery.Soc = 84
	site.battery.Capacity = 10
	soc := 84.0
	cap := 10.0
	site.battery.Devices = []types.Measurement{{Soc: &soc, Capacity: &cap}}
	site.batterySocUpdated = []time.Time{time.Now()}

	site.updateBatteryMeters()

	assert.Equal(t, 84.0, site.battery.Soc, "soc retained when the read fails")
	assert.Equal(t, 84.0, *site.battery.Devices[0].Soc, "recent per-device soc retained when the read fails")
	require.NotNil(t, site.battery.Devices[0].Capacity, "capacity weighs the retained soc")
	assert.Equal(t, 10.0, *site.battery.Devices[0].Capacity)
	assert.Equal(t, 10.0, site.battery.Capacity, "aggregate capacity retained with the soc")
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
		batCon := batteryControllerMock(ctrl)

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
		site.updateBatteryMode(false, false, api.Rate{})

		if tc.internal != api.BatteryNormal {
			assert.Equal(t, tc.expected, site.batteryMode)
		}

		ctrl.Finish()
	}
}

// battery controller supporting the modes a soc limit can implement
func batteryControllerMock(ctrl *gomock.Controller) *api.MockBatteryController {
	batCon := api.NewMockBatteryController(ctrl)
	batCon.EXPECT().BatteryModes().Return([]api.BatteryMode{api.BatteryNormal, api.BatteryHold, api.BatteryCharge, api.BatteryDischarge}).AnyTimes()

	return batCon
}

// battery meter with soc, controller and soc limits
func batteryControlMock(ctrl *gomock.Controller, soc, maxSoc float64) (api.Meter, *api.MockBatteryController) {
	batSoc := api.NewMockBattery(ctrl)
	batSoc.EXPECT().Soc().Return(soc, nil).AnyTimes()

	batSocLimit := api.NewMockBatterySocLimiter(ctrl)
	batSocLimit.EXPECT().GetSocLimits().Return(0.0, maxSoc).AnyTimes()

	batCon := batteryControllerMock(ctrl)

	return &struct {
		api.Meter
		api.Battery
		api.BatteryController
		api.BatterySocLimiter
	}{
		Battery:           batSoc,
		BatteryController: batCon,
		BatterySocLimiter: batSocLimit,
	}, batCon
}

// TestBatteryHoldAppliedOnce guards that reaching max soc during grid charge switches
// the battery to hold mode once instead of on every update
func TestBatteryHoldAppliedOnce(t *testing.T) {
	ctrl := gomock.NewController(t)

	bat, batCon := batteryControlMock(ctrl, 90, 80)

	site := &Site{
		log:           util.NewLogger("foo"),
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{Name: "bat"}, bat)},
		batteryMode:   api.BatteryCharge,
	}

	batCon.EXPECT().SetBatteryMode(api.BatteryHold).Times(1)

	for range 3 {
		site.updateBatteryMode(true, false, api.Rate{})
	}

	ctrl.Finish()
}

// TestBatteryHoldNotShared guards that one battery reaching max soc does not put the
// remaining batteries into hold mode
func TestBatteryHoldNotShared(t *testing.T) {
	ctrl := gomock.NewController(t)

	full, fullCon := batteryControlMock(ctrl, 90, 80)
	empty, emptyCon := batteryControlMock(ctrl, 50, 80)

	site := &Site{
		log: util.NewLogger("foo"),
		batteryMeters: []config.Device[api.Meter]{
			config.NewStaticDevice(config.Named{Name: "full"}, full),
			config.NewStaticDevice(config.Named{Name: "empty"}, empty),
		},
		batteryMode: api.BatteryCharge,
	}

	fullCon.EXPECT().SetBatteryMode(api.BatteryHold).Times(1)
	emptyCon.EXPECT().SetBatteryMode(gomock.Any()).Times(0)

	site.updateBatteryMode(true, false, api.Rate{})

	ctrl.Finish()
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

	assert.Equal(t, api.BatteryHold, site.requiredBatteryMode(false, false, api.Rate{}))
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

	assert.Equal(t, api.BatteryCharge, site.requiredBatteryMode(true, false, api.Rate{}))
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
	modeController := batteryControllerMock(ctrl)

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
	modeController := batteryControllerMock(ctrl)

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
	first := batteryControllerMock(ctrl)
	second := batteryControllerMock(ctrl)

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
	modeController := batteryControllerMock(ctrl)

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
	modeController := batteryControllerMock(ctrl)
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
	modeController := batteryControllerMock(ctrl)
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

	modeReady := site.updateBatteryMode(true, false, api.Rate{})
	assert.False(t, modeReady)
	site.updateBatteryPowerControlPolicyWithSnapshot(api.Rate{}, modeReady, batteryChargeControlSnapshot{})
	assert.Equal(t, batteryPowerReleased, site.batteryPowerRegulator.phase)

	modeController.EXPECT().SetBatteryMode(api.BatteryNormal)
	modeReady = site.updateBatteryMode(false, false, api.Rate{})
	assert.True(t, modeReady)
	assert.False(t, site.batteryModeHandoffFailed)
}

// TestFailedBatteryModeWriteReappliesNormal guards that a failed mode write
// invalidates the cached mode: the write may have applied partially, so the
// withdrawal must write Normal explicitly before continuous control resumes.
func TestFailedBatteryModeWriteReappliesNormal(t *testing.T) {
	ctrl := gomock.NewController(t)
	powerController := api.NewMockBatteryPowerController(ctrl)
	modeController := batteryControllerMock(ctrl)
	batterySoc := api.NewMockBattery(ctrl)
	batterySoc.EXPECT().Soc().Return(50.0, nil).AnyTimes()

	var bat api.Meter = &struct {
		api.Meter
		api.Battery
		api.BatteryController
		api.BatteryPowerController
		api.BatteryPowerLimiter
		api.BatterySocLimiter
	}{
		Battery:                batterySoc,
		BatteryController:      modeController,
		BatteryPowerController: powerController,
		BatteryPowerLimiter:    testBatteryPowerLimiter{charge: 5000, discharge: 5000},
		BatterySocLimiter:      testBatterySocLimiter{min: 20, max: 95},
	}
	devices := []config.Device[api.Meter]{
		config.NewStaticDevice(config.Named{Name: "battery"}, bat),
	}
	site := &Site{
		log:               util.NewLogger("foo"),
		gridMeter:         config.NewStaticDevice[api.Meter](config.Named{Name: "grid"}, &regulatorTestMeter{}),
		batteryMeters:     devices,
		batteryMode:       api.BatteryHold,
		batterySocUpdated: []time.Time{time.Now()},
	}
	soc := 50.0
	site.battery.Devices = []types.Measurement{{Soc: &soc}}
	site.batteryPowerRegulator = newBatteryPowerRegulator(site.log, site.gridMeter.Instance(), devices)

	gomock.InOrder(
		powerController.EXPECT().SetBatteryPower(0.0),
		modeController.EXPECT().SetBatteryMode(api.BatteryNormal),
		modeController.EXPECT().SetBatteryMode(api.BatteryCharge).Return(errors.New("mode failed")),
		modeController.EXPECT().SetBatteryMode(api.BatteryNormal),
		powerController.EXPECT().SetBatteryPower(0.0),
	)

	require.True(t, site.updateBatteryMode(false, false, api.Rate{}), "normal is applied and cached")
	require.Equal(t, api.BatteryNormal, site.batteryModeApplied["battery"])

	require.False(t, site.updateBatteryMode(true, false, api.Rate{}), "charge write fails")
	assert.NotContains(t, site.batteryModeApplied, "battery", "a failed write leaves an unproven mode")

	modeReady := site.updateBatteryMode(false, false, api.Rate{})
	require.True(t, modeReady, "withdrawal restores normal")
	assert.Equal(t, api.BatteryNormal, site.batteryModeApplied["battery"])

	site.updateBatteryPowerControlPolicyWithSnapshot(api.Rate{}, modeReady, batteryChargeControlSnapshot{})
	assert.Equal(t, batteryPowerNeutral, site.batteryPowerRegulator.phase)
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

		mode := site.requiredBatteryMode(false, false, api.Rate{})
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
		batCon := batteryControllerMock(ctrl)

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
		site.updateBatteryMode(false, false, api.Rate{})
		if !ctrl.Satisfied() {
			ctrl.Finish()
		}

		// 3. verify required external mode only applied once
		site.updateBatteryMode(false, false, api.Rate{})
		if !ctrl.Satisfied() {
			ctrl.Finish()
		}

		// 4. verify timer expiry
		site.batteryModeExternalTimer = site.batteryModeExternalTimer.Add(-time.Hour)
		site.batteryModeWatchdogExpired()

		// mode reverted to unknown, timer still active
		assert.Equal(t, site.batteryModeExternal, api.BatteryUnknown)
		assert.False(t, site.batteryModeExternalTimer.IsZero())

		// battery switched back to normal mode unless already applied in step 2
		if tc.expected != api.BatteryNormal {
			batCon.EXPECT().SetBatteryMode(api.BatteryNormal).Times(1)
		}
		site.updateBatteryMode(false, false, api.Rate{})

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
		batCon := batteryControllerMock(ctrl)
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

		site.updateBatteryMode(true, false, api.Rate{})

		ctrl.Finish()
	}
}

func TestForcedBatteryDischargeLimits(t *testing.T) {
	reserve := 20.0

	for _, tc := range []struct {
		internal, expected api.BatteryMode
		soc                float64
	}{
		{api.BatteryUnknown, api.BatteryDischarge, 50},
		{api.BatteryUnknown, api.BatteryHold, 10},

		{api.BatteryNormal, api.BatteryDischarge, 50},
		{api.BatteryNormal, api.BatteryHold, 10},

		{api.BatteryHold, api.BatteryDischarge, 50},
		{api.BatteryHold, api.BatteryHold, 10}, // TODO make this api.BatteryUnknown

		{api.BatteryDischarge, api.BatteryUnknown, 50},
		// steady-state re-validation: mode already Discharge and unchanged, reserve
		// reached mid-cycle; applyBatteryMode must still run to catch the reserve
		{api.BatteryDischarge, api.BatteryHold, 10},
	} {
		t.Logf("%+v", tc)

		ctrl := gomock.NewController(t)

		var bat api.Meter
		batSoc := api.NewMockBattery(ctrl)
		batCon := batteryControllerMock(ctrl)
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
		batSocLimit.EXPECT().GetSocLimits().Return(reserve, 0.0).Times(1)

		if tc.expected != api.BatteryUnknown {
			batCon.EXPECT().SetBatteryMode(tc.expected).Times(1)
		}

		site.updateBatteryMode(false, true, api.Rate{})

		ctrl.Finish()
	}
}

// TestBatteryDischargeHemsCurtailed ensures grid discharge stops but the battery
// keeps serving house load while the grid operator curtails production.
func TestBatteryDischargeHemsCurtailed(t *testing.T) {
	ctrl := gomock.NewController(t)

	var bat api.Meter
	batCon := batteryControllerMock(ctrl)

	bat = &struct {
		api.Meter
		api.BatteryController
	}{
		Meter:             bat,
		BatteryController: batCon,
	}

	curtailed := 60
	hems := api.NewMockHEMS(ctrl)
	hems.EXPECT().CurtailedPercent().Return(&curtailed).AnyTimes()
	hems.EXPECT().MaxConsumptionPower().Return(nil).AnyTimes()

	site := &Site{
		log:           util.NewLogger("foo"),
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{}, bat)},
		hems:          hems,
	}

	batCon.EXPECT().SetBatteryMode(api.BatteryNormal).Times(1)

	site.updateBatteryMode(false, true, api.Rate{})

	ctrl.Finish()
}

// TestBatteryGridDischargeEvFastCharging ensures grid discharge is held back while an EV
// is fast charging, regardless of the (opt-in, off by default) batteryDischargeControl
// toggle - forcing the battery to sell while an EV needs a fast charge is a materially
// worse outcome than the toggle's original, softer self-consumption case.
func TestBatteryGridDischargeEvFastCharging(t *testing.T) {
	lp := &Loadpoint{status: api.StatusC, mode: api.ModeNow}

	site := &Site{
		log:           util.NewLogger("foo"),
		batteryMeters: []config.Device[api.Meter]{nil},
		loadpoints:    []*Loadpoint{lp},
	}

	res := site.requiredBatteryMode(false, true, api.Rate{})
	assert.Equal(t, api.BatteryHold, res, "expected discharge to be held back for a fast charging EV")
}

// TestBatteryGridDischargeActive ensures the limit only acts once the experimental
// grid discharge setting is enabled.
func TestBatteryGridDischargeActive(t *testing.T) {
	limit := 0.2
	rate := api.Rate{Value: 0.3}

	site := &Site{
		log:                       util.NewLogger("foo"),
		batteryGridDischargeLimit: &limit,
	}

	assert.False(t, site.batteryGridDischargeActive(rate), "expected limit to be inert while grid discharge is disabled")

	site.batteryGridDischarge = true
	assert.True(t, site.batteryGridDischargeActive(rate), "expected limit to act once grid discharge is enabled")
}

// TestBatteryGridDischargeLimitRequiresOptIn ensures a limit cannot outlive the
// experimental grid discharge setting: it is refused while the setting is off and
// dropped when the setting is turned off.
func TestBatteryGridDischargeLimitRequiresOptIn(t *testing.T) {
	ctrl := gomock.NewController(t)

	var bat api.Meter = &struct {
		api.Meter
		api.BatteryController
	}{
		BatteryController: api.NewMockBatteryController(ctrl),
	}

	site := &Site{
		log:           util.NewLogger("foo"),
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{}, bat)},
	}

	limit := 0.2

	assert.ErrorIs(t, site.SetBatteryGridDischargeLimit(&limit), ErrBatteryGridDischargeNotAvailable)
	assert.Nil(t, site.GetBatteryGridDischargeLimit(), "expected limit to be refused while grid discharge is disabled")

	assert.NoError(t, site.SetBatteryGridDischarge(true))
	assert.NoError(t, site.SetBatteryGridDischargeLimit(&limit))
	assert.Equal(t, &limit, site.GetBatteryGridDischargeLimit())

	assert.NoError(t, site.SetBatteryGridDischarge(false))
	assert.Nil(t, site.GetBatteryGridDischargeLimit(), "expected limit to be dropped with grid discharge")
}

// TestEvFastChargingActiveDisabledLoadpoint guards against the nil entries
// Loadpoints() returns for disabled loadpoints
func TestEvFastChargingActiveDisabledLoadpoint(t *testing.T) {
	site := &Site{loadpoints: []*Loadpoint{nil}}

	assert.False(t, site.evFastChargingActive())
}
