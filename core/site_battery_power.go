package core

import (
	"errors"
	"math"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/hems/hems"
)

func (site *Site) hasBatteryPowerControl() bool {
	if site.batteryPowerRegulator != nil {
		return true
	}

	for _, dev := range site.batteryMeters {
		if api.HasCap[api.BatteryPowerController](dev.Instance()) {
			return true
		}
	}

	return false
}

func (site *Site) stopBatteryPowerControl() error {
	if site.batteryPowerRegulator != nil {
		return site.batteryPowerRegulator.releaseForHandoff()
	}

	var res error
	for _, dev := range site.batteryMeters {
		ctrl, ok := api.Cap[api.BatteryPowerController](dev.Instance())
		if !ok {
			continue
		}

		if err := ctrl.SetBatteryPower(0); err != nil {
			res = errors.Join(res, err)
		}
	}

	site.batteryPowerReleased = res == nil
	return res
}

func (site *Site) updateBatteryPowerControlPolicy(rate api.Rate, modeReady bool) {
	site.updateBatteryPowerControlPolicyWithSnapshot(rate, modeReady, batteryChargeControlSnapshot{})
}

func (site *Site) updateBatteryPowerControlPolicyWithSnapshot(
	rate api.Rate,
	modeReady bool,
	snapshot batteryChargeControlSnapshot,
) {
	if site.batteryPowerRegulator == nil {
		if modeReady && site.hasBatteryPowerControl() && !site.batteryPowerReleased {
			if err := site.stopBatteryPowerControl(); err != nil {
				site.log.ERROR.Printf("battery power control fallback: %v", err)
			}
		}
		return
	}

	mode := site.batteryPowerControlMode()
	dischargeControl := site.dischargeControlActive(rate)

	var chargePolicy batteryPowerControlPolicy
	if snapshot.ready && snapshot.mode == mode {
		chargePolicy = snapshot.policy
	} else {
		chargePolicy = site.batteryChargeControlPolicy(mode)
	}

	policy := batteryPowerControlPolicyWithDischarge(chargePolicy, mode, dischargeControl)
	policy.valid = policy.valid && modeReady
	if err := site.batteryPowerRegulator.setPolicy(policy); err != nil {
		site.log.ERROR.Printf("battery power control policy: %v", err)
	}
}

func (site *Site) batteryPowerControlPolicy(rate api.Rate) batteryPowerControlPolicy {
	mode := site.batteryPowerControlMode()
	dischargeControl := site.dischargeControlActive(rate)
	return batteryPowerControlPolicyWithDischarge(site.batteryChargeControlPolicy(mode), mode, dischargeControl)
}

func batteryPowerControlPolicyWithDischarge(
	policy batteryPowerControlPolicy,
	mode api.BatteryMode,
	dischargeControl bool,
) batteryPowerControlPolicy {
	if !policy.active || !policy.socLimitsValid {
		return policy
	}

	policy.dischargeAllowed = mode != api.BatteryHold &&
		mode != api.BatteryCharge &&
		!dischargeControl &&
		policy.soc > policy.minSoc

	return policy
}

type batteryChargeControlSnapshot struct {
	mode   api.BatteryMode
	policy batteryPowerControlPolicy
	ready  bool
}

func (site *Site) batteryPowerControlMode() api.BatteryMode {
	mode := site.GetBatteryMode()
	if externalMode := site.GetBatteryModeExternal(); externalMode != api.BatteryUnknown {
		return externalMode
	}
	return mode
}

// batteryChargeControlPolicy returns current charge eligibility without evaluating
// or mutating the independent discharge policy.
func (site *Site) batteryChargeControlPolicy(mode api.BatteryMode) batteryPowerControlPolicy {
	residualPower := site.GetResidualPower()
	policy := batteryPowerControlPolicy{
		valid:         !invalidBatteryPowerValue(residualPower),
		residualPower: residualPower,
		forceCharge:   mode == api.BatteryCharge,
	}

	dimmed := hems.Dimmed(site.hems)
	regulated := site.batteryPowerRegulator.battery
	meter := regulated.meter

	if batteryModeModified(mode) && api.HasCap[api.BatteryController](meter) {
		return policy
	}

	limits, ok := api.Cap[api.BatteryPowerLimiter](meter)
	if !ok {
		site.log.ERROR.Printf("battery %s power control: missing power limits", regulated.name)
		return policy
	}

	chargeLimit, dischargeLimit := limits.GetPowerLimits()
	if invalidBatteryPowerValue(chargeLimit) || invalidBatteryPowerValue(dischargeLimit) {
		site.log.ERROR.Printf("battery %s power control: invalid power limits", regulated.name)
		return policy
	}
	policy.active = chargeLimit > 0 || dischargeLimit > 0
	policy.chargeAllowed = mode != api.BatteryHoldCharge
	policy.chargeLimit = math.Max(0, chargeLimit)
	policy.dischargeLimit = math.Max(0, dischargeLimit)
	if !policy.active {
		return policy
	}
	if mode == api.BatteryCharge {
		policy.chargeAllowed = true
	}
	if dimmed != nil && *dimmed {
		policy.chargeAllowed = false
	}

	limiter, ok := api.Cap[api.BatterySocLimiter](meter)
	if !ok {
		site.log.ERROR.Printf("battery %s power control: missing soc limits", regulated.name)
		policy.active = false
		policy.chargeAllowed = false
		return policy
	}

	if regulated.siteIndex >= len(site.battery.Devices) {
		site.log.ERROR.Printf("battery %s power control: missing soc measurement", regulated.name)
		policy.chargeAllowed = false
	} else if soc := site.battery.Devices[regulated.siteIndex].Soc; soc == nil || invalidBatteryPowerValue(*soc) {
		site.log.ERROR.Printf("battery %s power control: invalid soc", regulated.name)
		policy.chargeAllowed = false
	} else {
		minSoc, maxSoc := limiter.GetSocLimits()
		if invalidBatteryPowerValue(minSoc) || invalidBatteryPowerValue(maxSoc) {
			site.log.ERROR.Printf("battery %s power control: invalid soc limits", regulated.name)
			policy.chargeAllowed = false
			return policy
		}
		if maxSoc == 0 {
			maxSoc = 100
		}

		policy.soc = *soc
		policy.minSoc = minSoc
		policy.maxSoc = maxSoc
		policy.socLimitsValid = true
		if regulated.siteIndex < len(site.batterySocUpdated) {
			policy.socUpdatedAt = site.batterySocUpdated[regulated.siteIndex]
		}
		policy.chargeAllowed = policy.chargeAllowed && *soc < maxSoc
	}

	return policy
}

func invalidBatteryPowerValue(value float64) bool {
	return math.IsNaN(value) || math.IsInf(value, 0)
}
