package core

import (
	"errors"
	"math"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/hems/hems"
	"github.com/evcc-io/evcc/util/config"
)

type batteryPowerControlCandidate struct {
	dev              config.Device[api.Meter]
	ctrl             api.BatteryPowerController
	power            float64
	chargeLimit      float64
	dischargeLimit   float64
	chargeAllowed    bool
	dischargeAllowed bool
}

func (site *Site) hasBatteryPowerControl() bool {
	for _, dev := range site.batteryMeters {
		if api.HasCap[api.BatteryPowerController](dev.Instance()) {
			return true
		}
	}

	return false
}

func (site *Site) stopBatteryPowerControl() error {
	var res error

	for _, dev := range site.batteryMeters {
		ctrl, ok := api.Cap[api.BatteryPowerController](dev.Instance())
		if !ok {
			continue
		}

		if err := ctrl.SetBatteryPower(0); err != nil && !errors.Is(err, api.ErrNotAvailable) {
			res = errors.Join(res, err)
		}
	}

	return res
}

func (site *Site) updateBatteryPowerControl() {
	if !site.hasBatteryPowerControl() || site.gridMeter == nil {
		return
	}

	if !site.gridPowerFresh {
		if err := site.stopBatteryPowerControl(); err != nil {
			site.log.ERROR.Println("battery power control:", err)
		}
		return
	}

	gridPower := site.gridPower
	if invalidBatteryPowerValue(gridPower) {
		site.log.ERROR.Printf("battery power control: invalid grid power: %.0fW", gridPower)
		if err := site.stopBatteryPowerControl(); err != nil {
			site.log.ERROR.Println("battery power control:", err)
		}
		return
	}

	mode := site.GetBatteryMode()
	if externalMode := site.GetBatteryModeExternal(); externalMode != api.BatteryUnknown {
		mode = externalMode
	}
	candidates := site.batteryPowerControlCandidates(mode)
	if len(candidates) == 0 {
		return
	}

	if dimmed := hems.Dimmed(site.hems); dimmed != nil && *dimmed {
		for i := range candidates {
			candidates[i].chargeAllowed = false
		}
	}

	target := site.batteryPowerTarget(mode, gridPower, candidates)
	targets := distributeBatteryPowerTarget(target, candidates)

	for i, candidate := range candidates {
		target := targets[i]
		if err := candidate.ctrl.SetBatteryPower(target); err != nil {
			if !errors.Is(err, api.ErrNotAvailable) {
				site.log.ERROR.Printf("battery %s power control: %v", deviceTitleOrName(candidate.dev), err)
			}
			continue
		}

		site.log.DEBUG.Printf("set battery %s power: %.0fW", deviceTitleOrName(candidate.dev), target)
	}
}

func invalidBatteryPowerValue(v float64) bool {
	return math.IsNaN(v) || math.IsInf(v, 0)
}

func (site *Site) batteryPowerControlCandidates(mode api.BatteryMode) []batteryPowerControlCandidate {
	consumption, err := site.tariffRates(api.TariffUsagePlanner)
	if err != nil {
		site.log.WARN.Println("planner:", err)
	}

	var rate api.Rate
	if consumption != nil {
		rate, _ = consumption.At(time.Now())
	}

	dischargeControl := site.dischargeControlActive(rate)

	chargeAllowed := mode != api.BatteryHoldCharge
	dischargeAllowed := mode != api.BatteryHold && !dischargeControl

	if mode == api.BatteryCharge {
		chargeAllowed = true
		dischargeAllowed = false
	}

	candidates := make([]batteryPowerControlCandidate, 0, len(site.batteryMeters))

	for i, dev := range site.batteryMeters {
		meter := dev.Instance()

		ctrl, ok := api.Cap[api.BatteryPowerController](meter)
		if !ok {
			continue
		}

		if mode != api.BatteryUnknown && mode != api.BatteryNormal && api.HasCap[api.BatteryController](meter) {
			continue
		}

		limits, ok := api.Cap[api.BatteryPowerLimiter](meter)
		if !ok {
			site.log.ERROR.Printf("battery %s power control: missing power limits", deviceTitleOrName(dev))
			site.stopBatteryPowerControlCandidate(dev, ctrl)
			continue
		}

		chargeLimit, dischargeLimit := limits.GetPowerLimits()
		if invalidBatteryPowerValue(chargeLimit) || invalidBatteryPowerValue(dischargeLimit) || chargeLimit <= 0 && dischargeLimit <= 0 {
			site.log.ERROR.Printf("battery %s power control: invalid power limits", deviceTitleOrName(dev))
			site.stopBatteryPowerControlCandidate(dev, ctrl)
			continue
		}

		if i >= len(site.battery.Devices) {
			site.log.ERROR.Printf("battery %s power control: missing measurement", deviceTitleOrName(dev))
			site.stopBatteryPowerControlCandidate(dev, ctrl)
			continue
		}

		measurement := site.battery.Devices[i]
		if i >= len(site.batteryPowerFresh) || !site.batteryPowerFresh[i] {
			site.log.ERROR.Printf("battery %s power control: stale power", deviceTitleOrName(dev))
			site.stopBatteryPowerControlCandidate(dev, ctrl)
			continue
		}

		if invalidBatteryPowerValue(measurement.Power) {
			site.log.ERROR.Printf("battery %s power control: invalid power: %.0fW", deviceTitleOrName(dev), measurement.Power)
			site.stopBatteryPowerControlCandidate(dev, ctrl)
			continue
		}

		candidate := batteryPowerControlCandidate{
			dev:              dev,
			ctrl:             ctrl,
			power:            measurement.Power,
			chargeLimit:      math.Max(0, chargeLimit),
			dischargeLimit:   math.Max(0, dischargeLimit),
			chargeAllowed:    chargeAllowed,
			dischargeAllowed: dischargeAllowed,
		}

		if limiter, ok := api.Cap[api.BatterySocLimiter](meter); ok {
			if measurement.Soc == nil || invalidBatteryPowerValue(*measurement.Soc) {
				site.log.ERROR.Printf("battery %s power control: invalid soc", deviceTitleOrName(dev))
				site.stopBatteryPowerControlCandidate(dev, ctrl)
				continue
			}

			minSoc, maxSoc := limiter.GetSocLimits()
			if maxSoc == 0 {
				maxSoc = 100
			}

			candidate.chargeAllowed = candidate.chargeAllowed && *measurement.Soc < maxSoc
			candidate.dischargeAllowed = candidate.dischargeAllowed && *measurement.Soc > minSoc
		}

		candidates = append(candidates, candidate)
	}

	return candidates
}

func (site *Site) stopBatteryPowerControlCandidate(dev config.Device[api.Meter], ctrl api.BatteryPowerController) {
	if err := ctrl.SetBatteryPower(0); err != nil {
		if !errors.Is(err, api.ErrNotAvailable) {
			site.log.ERROR.Printf("battery %s power control: %v", deviceTitleOrName(dev), err)
		}
		return
	}
}

func (site *Site) batteryPowerTarget(mode api.BatteryMode, gridPower float64, candidates []batteryPowerControlCandidate) float64 {
	var maxCharge, maxDischarge float64
	for _, candidate := range candidates {
		if candidate.chargeAllowed {
			maxCharge += candidate.chargeLimit
		}
		if candidate.dischargeAllowed {
			maxDischarge += candidate.dischargeLimit
		}
	}

	if mode == api.BatteryCharge {
		return -maxCharge
	}

	var currentPower float64
	for _, candidate := range candidates {
		currentPower += candidate.power
	}

	target := math.Round(currentPower + gridPower)
	switch {
	case target < 0:
		if maxCharge <= 0 {
			return 0
		}
		return math.Max(target, -maxCharge)

	case target > 0:
		if maxDischarge <= 0 {
			return 0
		}
		return math.Min(target, maxDischarge)

	default:
		return 0
	}
}

func distributeBatteryPowerTarget(target float64, candidates []batteryPowerControlCandidate) []float64 {
	targets := make([]float64, len(candidates))
	if target == 0 {
		return targets
	}

	charge := target < 0
	var totalLimit float64
	for _, candidate := range candidates {
		if charge && candidate.chargeAllowed {
			totalLimit += candidate.chargeLimit
		}
		if !charge && candidate.dischargeAllowed {
			totalLimit += candidate.dischargeLimit
		}
	}

	if totalLimit <= 0 {
		return targets
	}

	var assigned float64
	last := -1
	for i, candidate := range candidates {
		var limit float64
		switch {
		case charge && candidate.chargeAllowed:
			limit = candidate.chargeLimit
		case !charge && candidate.dischargeAllowed:
			limit = candidate.dischargeLimit
		}

		if limit <= 0 {
			continue
		}

		value := math.Round(target * limit / totalLimit)
		targets[i] = value
		assigned += value
		last = i
	}

	if last >= 0 {
		targets[last] += target - assigned
	}

	return targets
}
