package core

import (
	"sync"
	"time"

	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/types"
)

type liveMeterState struct {
	mu               sync.Mutex
	publish          func(string, any)
	grid             types.Measurement
	gridReadAt       time.Time
	battery          types.BatteryState
	batteryReadAt    []time.Time
	pvPower          float64
	totalChargePower float64
}

func (state *liveMeterState) updateGrid(measurement types.Measurement, reading powerReading) {
	state.mu.Lock()
	defer state.mu.Unlock()

	next := cloneMeasurement(measurement)
	powerValid := reading.valid && !invalidBatteryPowerValue(next.Power)
	if !state.gridReadAt.IsZero() && !reading.readAt.After(state.gridReadAt) {
		next.Power = state.grid.Power
	} else if powerValid {
		state.gridReadAt = reading.readAt
	} else {
		next.Power = 0
		state.gridReadAt = reading.readAt
	}
	state.grid = next
	state.send(keys.Grid, cloneMeasurement(next))
}

func (state *liveMeterState) updateBattery(battery types.BatteryState, power []powerReading) {
	state.mu.Lock()
	defer state.mu.Unlock()

	next := cloneBatteryState(battery)
	nextReadAt := make([]time.Time, len(next.Devices))
	for i := range next.Devices {
		var reading *powerReading
		if i < len(power) {
			reading = &power[i]
		}

		hasCurrent := i < len(state.battery.Devices) && i < len(state.batteryReadAt)
		switch {
		case reading == nil && hasCurrent:
			next.Devices[i].Power = state.battery.Devices[i].Power
			nextReadAt[i] = state.batteryReadAt[i]
		case reading == nil:
			next.Devices[i].Power = 0
		case hasCurrent && !reading.readAt.After(state.batteryReadAt[i]):
			next.Devices[i].Power = state.battery.Devices[i].Power
			nextReadAt[i] = state.batteryReadAt[i]
		case reading.valid && !invalidBatteryPowerValue(next.Devices[i].Power):
			nextReadAt[i] = reading.readAt
		default:
			next.Devices[i].Power = 0
			nextReadAt[i] = reading.readAt
		}
	}

	next.Power = batteryDevicePower(next.Devices)
	state.battery = next
	state.batteryReadAt = nextReadAt
	state.send(keys.Battery, cloneBatteryState(next))
}

func (state *liveMeterState) observe(observation batteryPowerObservation) {
	state.mu.Lock()
	defer state.mu.Unlock()

	gridUpdated := observation.Grid.Valid &&
		!invalidBatteryPowerValue(observation.Grid.Power) &&
		observation.Grid.FinishedAt.After(state.gridReadAt)
	if gridUpdated {
		state.grid.Power = observation.Grid.Power
		state.gridReadAt = observation.Grid.FinishedAt
	}

	batteryUpdated := false
	if observation.Battery.Valid &&
		!invalidBatteryPowerValue(observation.Battery.Power) &&
		observation.BatteryIndex >= 0 &&
		observation.BatteryIndex < len(state.battery.Devices) &&
		observation.BatteryIndex < len(state.batteryReadAt) &&
		observation.Battery.FinishedAt.After(state.batteryReadAt[observation.BatteryIndex]) {
		state.battery.Devices[observation.BatteryIndex].Power = observation.Battery.Power
		state.battery.Power = batteryDevicePower(state.battery.Devices)
		state.batteryReadAt[observation.BatteryIndex] = observation.Battery.FinishedAt
		batteryUpdated = true
	}

	if gridUpdated {
		state.send(keys.Grid, cloneMeasurement(state.grid))
	}
	if batteryUpdated {
		state.send(keys.Battery, cloneBatteryState(state.battery))
	}
	if gridUpdated || batteryUpdated {
		state.send(keys.HomePower, state.homePower())
	}
}

func (state *liveMeterState) setPVPower(power float64) {
	state.mu.Lock()
	defer state.mu.Unlock()

	state.pvPower = power
}

func (state *liveMeterState) setChargePower(power float64) {
	state.mu.Lock()
	defer state.mu.Unlock()

	state.totalChargePower = power
}

func (state *liveMeterState) publishHome() {
	state.mu.Lock()
	defer state.mu.Unlock()

	state.send(keys.HomePower, state.homePower())
}

func (state *liveMeterState) send(key string, value any) {
	if state.publish != nil {
		state.publish(key, value)
	}
}

func (state *liveMeterState) homePower() float64 {
	return max(0, state.grid.Power+max(0, state.pvPower)+state.battery.Power-state.totalChargePower)
}

func batteryDevicePower(devices []types.Measurement) float64 {
	var power float64
	for _, device := range devices {
		power += device.Power
	}
	return power
}

func cloneMeasurement(measurement types.Measurement) types.Measurement {
	res := measurement
	res.Energy = cloneValue(measurement.Energy)
	res.ReturnEnergy = cloneValue(measurement.ReturnEnergy)
	res.Powers = append([]float64(nil), measurement.Powers...)
	res.Currents = append([]float64(nil), measurement.Currents...)
	res.Capacity = cloneValue(measurement.Capacity)
	res.Soc = cloneValue(measurement.Soc)
	res.Controllable = cloneValue(measurement.Controllable)
	res.Suggestion = cloneValue(measurement.Suggestion)
	return res
}

func cloneBatteryState(state types.BatteryState) types.BatteryState {
	res := state
	res.Devices = make([]types.Measurement, len(state.Devices))
	for i, device := range state.Devices {
		res.Devices[i] = cloneMeasurement(device)
	}
	if state.Forecast != nil {
		forecast := *state.Forecast
		forecast.Highest = cloneValue(state.Forecast.Highest)
		forecast.Lowest = cloneValue(state.Forecast.Lowest)
		res.Forecast = &forecast
	}
	return res
}

func cloneValue[T any](value *T) *T {
	if value == nil {
		return nil
	}

	res := *value
	return &res
}
