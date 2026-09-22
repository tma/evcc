package core

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/types"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBatteryPowerRegulatorPublishesLiveMeterState(t *testing.T) {
	gridMeter := &regulatorTestMeter{power: 300}
	otherBattery := &regulatorTestMeter{power: 400}
	controlledMeter := &regulatorTestMeter{power: -250}
	controller := &regulatorTestController{}

	var controlled api.Meter = &struct {
		api.Meter
		api.BatteryPowerController
	}{
		Meter:                  controlledMeter,
		BatteryPowerController: controller,
	}
	batteryMeters := []config.Device[api.Meter]{
		config.NewStaticDevice(config.Named{Name: "other"}, api.Meter(otherBattery)),
		config.NewStaticDevice(config.Named{Name: "controlled"}, controlled),
	}

	valueChan := make(chan util.Param, 16)
	site := &Site{
		log:           util.NewLogger(t.Name()),
		valueChan:     valueChan,
		batteryMeters: batteryMeters,
		gridPower:     987,
		battery: types.BatteryState{
			Power:    654,
			Energy:   42,
			Capacity: 20,
			Soc:      73,
			Devices: []types.Measurement{
				{Name: "control-other", Power: 321},
				{Name: "control-controlled", Power: 333},
			},
		},
	}

	energy, returnEnergy := 12.3, 4.5
	soc, capacity, controllable := 73.0, 10.0, true
	suggestion := &types.Suggestion{Action: "hold", Charge: 800, Actionable: true}
	forecast := &types.BatteryForecast{
		Highest: &types.BatteryForecastPoint{Soc: 91, Time: time.Unix(10, 0), Limit: true},
		Lowest:  &types.BatteryForecastPoint{Soc: 36, Time: time.Unix(20, 0)},
	}
	clck := clock.NewMock()
	initialAt := clck.Now().Add(-time.Second)
	site.liveMeters = liveMeterState{
		publish: site.publish,
		grid: types.Measurement{
			Name:         "grid",
			Title:        "Grid title",
			Icon:         "grid-icon",
			Power:        900,
			Energy:       &energy,
			ReturnEnergy: &returnEnergy,
			Powers:       []float64{100, 200, 600},
			Currents:     []float64{1, 2, 3},
		},
		gridReadAt: initialAt,
		battery: types.BatteryState{
			Power:    700,
			Energy:   24,
			Capacity: 20,
			Soc:      73,
			Devices: []types.Measurement{
				{Name: "other", Title: "Other battery", Icon: "battery-1", Power: 400, Soc: &soc},
				{
					Name:         "controlled",
					Title:        "Controlled battery",
					Icon:         "battery-2",
					Power:        300,
					Energy:       &energy,
					ReturnEnergy: &returnEnergy,
					Capacity:     &capacity,
					Soc:          &soc,
					Controllable: &controllable,
					Suggestion:   suggestion,
				},
			},
			Forecast: forecast,
		},
		batteryReadAt:    []time.Time{initialAt, initialAt},
		pvPower:          1000,
		totalChargePower: 600,
	}

	regulator := newBatteryPowerRegulator(site.log, gridMeter, batteryMeters)
	require.NotNil(t, regulator)
	regulator.clock = clck
	regulator.setSampleObserver(site.liveMeters.observe)
	require.NoError(t, regulator.setPolicy(batteryPowerControlPolicy{
		valid:            true,
		active:           true,
		chargeAllowed:    true,
		dischargeAllowed: true,
		chargeLimit:      5000,
		dischargeLimit:   5000,
	}))
	controller.reset()
	clck.Add(batteryPowerControlInterval)

	regulator.tick()

	params := receiveSiteParams(t, valueChan, 3)
	publishedGrid := requireParam[types.Measurement](t, params, keys.Grid)
	assert.Equal(t, 300.0, publishedGrid.Power)
	assert.Equal(t, "Grid title", publishedGrid.Title)
	assert.Equal(t, []float64{100, 200, 600}, publishedGrid.Powers)
	assert.Equal(t, []float64{1, 2, 3}, publishedGrid.Currents)
	assert.Equal(t, 12.3, *publishedGrid.Energy)
	assert.Equal(t, 4.5, *publishedGrid.ReturnEnergy)

	publishedBattery := requireParam[types.BatteryState](t, params, keys.Battery)
	assert.Equal(t, 150.0, publishedBattery.Power)
	assert.Equal(t, 24.0, publishedBattery.Energy)
	assert.Equal(t, 20.0, publishedBattery.Capacity)
	assert.Equal(t, 73.0, publishedBattery.Soc)
	require.Len(t, publishedBattery.Devices, 2)
	assert.Equal(t, 400.0, publishedBattery.Devices[0].Power)
	assert.Equal(t, -250.0, publishedBattery.Devices[1].Power)
	assert.Equal(t, "Controlled battery", publishedBattery.Devices[1].Title)
	assert.Equal(t, 12.3, *publishedBattery.Devices[1].Energy)
	assert.Equal(t, 4.5, *publishedBattery.Devices[1].ReturnEnergy)
	assert.Equal(t, 10.0, *publishedBattery.Devices[1].Capacity)
	assert.Equal(t, 73.0, *publishedBattery.Devices[1].Soc)
	assert.Equal(t, controllable, *publishedBattery.Devices[1].Controllable)
	assert.Equal(t, suggestion, publishedBattery.Devices[1].Suggestion)
	assert.Equal(t, forecast, publishedBattery.Forecast)
	assert.Equal(t, 850.0, requireParam[float64](t, params, keys.HomePower))

	assert.Equal(t, 987.0, site.gridPower)
	assert.Equal(t, 654.0, site.battery.Power)
	assert.Equal(t, 321.0, site.battery.Devices[0].Power)
	assert.Equal(t, 333.0, site.battery.Devices[1].Power)

	site.liveMeters.setPVPower(2000)
	site.liveMeters.setChargePower(1000)
	site.liveMeters.observe(batteryPowerObservation{
		Grid: batteryPowerObservationSample{
			Power:      999,
			FinishedAt: clck.Now().Add(time.Second),
			Valid:      true,
		},
		Battery: batteryPowerObservationSample{
			Power:      888,
			FinishedAt: clck.Now().Add(time.Second),
			Valid:      true,
		},
		BatteryIndex: 1,
	})
	updatedParams := receiveSiteParams(t, valueChan, 3)
	assert.Equal(t, 3287.0, requireParam[float64](t, updatedParams, keys.HomePower))
	assert.Equal(t, 300.0, publishedGrid.Power, "published grid value must be immutable")
	assert.Equal(t, -250.0, publishedBattery.Devices[1].Power, "published battery value must be immutable")
}

func TestBatteryPowerRegulatorObservesSamplesIndependently(t *testing.T) {
	now := time.Unix(100, 0)
	valid := batteryPowerSample{Value: 200, StartedAt: now.Add(-time.Second), FinishedAt: now}

	for _, tc := range []struct {
		name                    string
		grid, battery           batteryPowerSample
		gridValid, batteryValid bool
	}{
		{
			name:         "invalid grid",
			grid:         batteryPowerSample{Value: math.NaN(), StartedAt: now, FinishedAt: now},
			battery:      valid,
			batteryValid: true,
		},
		{
			name:      "invalid battery",
			grid:      valid,
			battery:   batteryPowerSample{Value: math.Inf(1), StartedAt: now, FinishedAt: now},
			gridValid: true,
		},
		{
			name: "stale grid",
			grid: batteryPowerSample{
				Value:      100,
				StartedAt:  now.Add(-7 * time.Second),
				FinishedAt: now.Add(-6 * time.Second),
			},
			battery:      valid,
			batteryValid: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observations := make(chan batteryPowerObservation, 1)
			regulator := &batteryPowerRegulator{
				sampleObserver: func(observation batteryPowerObservation) {
					observations <- observation
				},
				sampleObserverEnabled: true,
			}

			regulator.notifySampleObserverLocked(now, tc.grid, tc.battery)

			select {
			case observation := <-observations:
				assert.Equal(t, tc.gridValid, observation.Grid.Valid)
				assert.Equal(t, tc.batteryValid, observation.Battery.Valid)
			case <-time.After(time.Second):
				t.Fatal("sample observer was not called")
			}
		})
	}
}

func TestLiveMeterStateRejectsOutOfOrderPower(t *testing.T) {
	valueChan := make(chan util.Param, 8)
	batteryMeters := []config.Device[api.Meter]{
		config.NewStaticDevice(config.Named{Name: "controlled"}, api.Meter(&regulatorTestMeter{})),
		config.NewStaticDevice(config.Named{Name: "other"}, api.Meter(&regulatorTestMeter{})),
	}
	site := &Site{
		log:           util.NewLogger(t.Name()),
		valueChan:     valueChan,
		batteryMeters: batteryMeters,
	}
	base := time.Unix(100, 0)
	site.liveMeters = liveMeterState{
		publish:       site.publish,
		grid:          types.Measurement{Name: "grid", Power: 100},
		gridReadAt:    base,
		battery:       types.BatteryState{Devices: []types.Measurement{{Name: "controlled", Power: -100}, {Name: "other", Power: 200}}},
		batteryReadAt: []time.Time{base, base},
	}

	site.liveMeters.observe(batteryPowerObservation{
		Grid:         batteryPowerObservationSample{Power: 300, FinishedAt: base.Add(2 * time.Second), Valid: true},
		Battery:      batteryPowerObservationSample{Power: -300, FinishedAt: base.Add(2 * time.Second), Valid: true},
		BatteryIndex: 0,
	})
	receiveSiteParams(t, valueChan, 3)

	energy := 9.0
	site.liveMeters.updateGrid(types.Measurement{
		Name:     "grid",
		Power:    50,
		Energy:   &energy,
		Powers:   []float64{10, 20, 20},
		Currents: []float64{1, 2, 2},
	}, validPowerReading(base.Add(time.Second)))
	gridParam := <-valueChan
	publishedGrid, ok := gridParam.Val.(types.Measurement)
	require.True(t, ok)
	assert.Equal(t, 300.0, publishedGrid.Power)
	assert.Equal(t, []float64{10, 20, 20}, publishedGrid.Powers)
	assert.Equal(t, 9.0, *publishedGrid.Energy)

	site.liveMeters.updateBattery(types.BatteryState{
		Energy:   30,
		Capacity: 20,
		Soc:      60,
		Devices: []types.Measurement{
			{Name: "controlled", Title: "Controlled", Power: -50},
			{Name: "other", Title: "Other", Power: 500},
		},
	}, validPowerReadings(base.Add(time.Second), base.Add(3*time.Second)))
	batteryParam := <-valueChan
	publishedBattery, ok := batteryParam.Val.(types.BatteryState)
	require.True(t, ok)
	assert.Equal(t, 200.0, publishedBattery.Power)
	assert.Equal(t, -300.0, publishedBattery.Devices[0].Power)
	assert.Equal(t, 500.0, publishedBattery.Devices[1].Power)
	assert.Equal(t, "Controlled", publishedBattery.Devices[0].Title)
	assert.Equal(t, 60.0, publishedBattery.Soc)

	site.liveMeters.updateGrid(types.Measurement{
		Name:  "grid",
		Power: math.NaN(),
	}, powerReading{readAt: base.Add(4 * time.Second)})
	gridParam = <-valueChan
	publishedGrid, ok = gridParam.Val.(types.Measurement)
	require.True(t, ok)
	assert.Zero(t, publishedGrid.Power)

	site.liveMeters.updateBattery(types.BatteryState{
		Devices: []types.Measurement{
			{Name: "controlled", Power: math.Inf(1)},
			{Name: "other", Power: 500},
		},
	}, []powerReading{
		{readAt: base.Add(4 * time.Second)},
		validPowerReading(base.Add(3 * time.Second)),
	})
	batteryParam = <-valueChan
	publishedBattery, ok = batteryParam.Val.(types.BatteryState)
	require.True(t, ok)
	assert.Equal(t, 500.0, publishedBattery.Power)
	assert.Zero(t, publishedBattery.Devices[0].Power)

	site.liveMeters.observe(batteryPowerObservation{
		Grid:         batteryPowerObservationSample{Power: math.NaN(), FinishedAt: base.Add(4 * time.Second), Valid: true},
		Battery:      batteryPowerObservationSample{Power: math.Inf(-1), FinishedAt: base.Add(4 * time.Second), Valid: true},
		BatteryIndex: 0,
	})
	site.liveMeters.observe(batteryPowerObservation{
		Grid:         batteryPowerObservationSample{Power: 1, FinishedAt: base, Valid: true},
		Battery:      batteryPowerObservationSample{Power: 1, FinishedAt: base, Valid: true},
		BatteryIndex: 0,
	})
	assert.Empty(t, valueChan)
}

func TestBatteryPowerRegulatorDoesNotPublishAfterRelease(t *testing.T) {
	batteryMeter := &blockingRegulatorTestMeter{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	gridMeter := &regulatorTestMeter{}
	controller := &regulatorTestController{}
	var battery api.Meter = &struct {
		api.Meter
		api.BatteryPowerController
	}{
		Meter:                  batteryMeter,
		BatteryPowerController: controller,
	}
	devices := []config.Device[api.Meter]{
		config.NewStaticDevice(config.Named{Name: "battery"}, battery),
	}
	regulator := newBatteryPowerRegulator(util.NewLogger(t.Name()), gridMeter, devices)
	require.NotNil(t, regulator)
	require.NoError(t, regulator.setPolicy(batteryPowerControlPolicy{
		valid:            true,
		active:           true,
		chargeAllowed:    true,
		dischargeAllowed: true,
		chargeLimit:      5000,
		dischargeLimit:   5000,
	}))
	observations := make(chan batteryPowerObservation, 1)
	regulator.setSampleObserver(func(observation batteryPowerObservation) {
		observations <- observation
	})

	done := make(chan struct{})
	go func() {
		regulator.tick()
		close(done)
	}()
	<-batteryMeter.started
	require.NoError(t, regulator.release())
	close(batteryMeter.release)
	<-done

	assert.Empty(t, observations)
}

func TestBatteryPowerRegulatorDoesNotPublishAfterFailedRelease(t *testing.T) {
	fixture := newRegulatorTestFixture(t, 300, 0, 100)
	observations := make(chan batteryPowerObservation, 1)
	fixture.regulator.setSampleObserver(func(observation batteryPowerObservation) {
		observations <- observation
	})

	fixture.step(0)
	<-observations

	fixture.controller.fail(errors.New("release failed"))
	require.Error(t, fixture.regulator.release())

	fixture.step(batteryPowerControlInterval)
	assert.Empty(t, observations)
}

func TestBatteryPowerRegulatorPublishesAfterAcquisitionRecovery(t *testing.T) {
	grid := &regulatorTestMeter{power: 300}
	batteryMeter := &regulatorTestMeter{}
	controller := &regulatorTestController{}
	controller.fail(errors.New("acquisition failed"))

	var battery api.Meter = &struct {
		api.Meter
		api.BatteryPowerController
	}{
		Meter:                  batteryMeter,
		BatteryPowerController: controller,
	}
	devices := []config.Device[api.Meter]{
		config.NewStaticDevice(config.Named{Name: "battery"}, battery),
	}
	regulator := newBatteryPowerRegulator(util.NewLogger(t.Name()), grid, devices)
	require.NotNil(t, regulator)
	clck := clock.NewMock()
	regulator.clock = clck
	observations := make(chan batteryPowerObservation, 1)
	regulator.setSampleObserver(func(observation batteryPowerObservation) {
		observations <- observation
	})

	require.Error(t, regulator.setPolicy(batteryPowerControlPolicy{
		valid:            true,
		active:           true,
		chargeAllowed:    true,
		dischargeAllowed: true,
		chargeLimit:      5000,
		dischargeLimit:   5000,
	}))

	clck.Add(batteryPowerControlInterval)
	regulator.tick()
	assert.Empty(t, observations)

	clck.Add(batteryPowerControlInterval)
	regulator.tick()
	select {
	case <-observations:
	case <-time.After(time.Second):
		t.Fatal("sample observer was not called after acquisition recovery")
	}
}

func TestBatteryPowerRegulatorObserverDoesNotBlockTick(t *testing.T) {
	fixture := newRegulatorTestFixture(t, 300, 0, 100)
	started := make(chan struct{})
	release := make(chan struct{})
	fixture.regulator.setSampleObserver(func(batteryPowerObservation) {
		close(started)
		<-release
	})

	done := make(chan struct{})
	go func() {
		fixture.step(0)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("sample observer was not called")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sample observer blocked the regulator tick")
	}
	close(release)
}

func TestLiveMeterStateConcurrentUpdates(t *testing.T) {
	batteryMeters := []config.Device[api.Meter]{
		config.NewStaticDevice(config.Named{Name: "controlled"}, api.Meter(&regulatorTestMeter{})),
		config.NewStaticDevice(config.Named{Name: "other"}, api.Meter(&regulatorTestMeter{})),
	}
	site := &Site{
		log:           util.NewLogger(t.Name()),
		batteryMeters: batteryMeters,
	}
	base := time.Unix(100, 0)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := range 100 {
			site.liveMeters.updateGrid(types.Measurement{
				Name:     "grid",
				Power:    float64(i),
				Powers:   []float64{float64(i), 0, 0},
				Currents: []float64{float64(i) / 230, 0, 0},
			}, validPowerReading(base.Add(time.Duration(i)*time.Second)))
		}
	}()
	go func() {
		defer wg.Done()
		for i := range 100 {
			site.liveMeters.updateBattery(types.BatteryState{
				Soc: float64(i),
				Devices: []types.Measurement{
					{Name: "controlled", Power: -float64(i)},
					{Name: "other", Power: float64(i * 2)},
				},
			}, validPowerReadings(
				base.Add(time.Duration(i)*time.Second),
				base.Add(time.Duration(i)*time.Second),
			))
			site.liveMeters.setPVPower(float64(i * 3))
			site.liveMeters.setChargePower(float64(i))
			site.liveMeters.publishHome()
		}
	}()
	go func() {
		defer wg.Done()
		for i := range 100 {
			site.liveMeters.observe(batteryPowerObservation{
				Grid: batteryPowerObservationSample{
					Power:      float64(i * 4),
					FinishedAt: base.Add(time.Duration(i)*time.Second + time.Millisecond),
					Valid:      true,
				},
				Battery: batteryPowerObservationSample{
					Power:      -float64(i * 5),
					FinishedAt: base.Add(time.Duration(i)*time.Second + time.Millisecond),
					Valid:      true,
				},
				BatteryIndex: 0,
			})
		}
	}()
	wg.Wait()

	site.liveMeters.mu.Lock()
	defer site.liveMeters.mu.Unlock()
	assert.Len(t, site.liveMeters.battery.Devices, 2)
	assert.False(t, invalidBatteryPowerValue(site.liveMeters.grid.Power))
	assert.False(t, invalidBatteryPowerValue(site.liveMeters.battery.Power))
	assert.False(t, invalidBatteryPowerValue(site.liveMeters.homePower()))
}

func receiveSiteParams(t *testing.T, valueChan <-chan util.Param, count int) map[string]any {
	t.Helper()

	res := make(map[string]any, count)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for range count {
		select {
		case param := <-valueChan:
			res[param.Key] = param.Val
		case <-timer.C:
			t.Fatalf("expected %d parameters, got %d", count, len(res))
		}
	}
	return res
}

func requireParam[T any](t *testing.T, params map[string]any, key string) T {
	t.Helper()

	value, ok := params[key]
	require.True(t, ok, "missing parameter %q", key)
	res, ok := value.(T)
	require.True(t, ok, "parameter %q has type %T", key, value)
	return res
}

func validPowerReading(readAt time.Time) powerReading {
	return powerReading{readAt: readAt, valid: true}
}

func validPowerReadings(readAt ...time.Time) []powerReading {
	res := make([]powerReading, len(readAt))
	for i, at := range readAt {
		res[i] = validPowerReading(at)
	}
	return res
}
