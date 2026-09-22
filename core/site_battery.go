package core

import (
	"errors"
	"slices"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/hems/hems"
	"github.com/evcc-io/evcc/util/config"
)

func batteryModeModified(mode api.BatteryMode) bool {
	return mode != api.BatteryUnknown && mode != api.BatteryNormal
}

func (site *Site) batteryConfigured() bool {
	return len(site.batteryMeters) > 0
}

func batteryControllable(meter api.Meter) bool {
	return api.HasCap[api.BatteryController](meter) || api.HasCap[api.BatteryPowerController](meter)
}

func (site *Site) hasBatteryControl() bool {
	for _, dev := range site.batteryMeters {
		if batteryControllable(dev.Instance()) {
			return true
		}
	}

	return false
}

// setBatteryMode sets the battery mode
func (site *Site) setBatteryMode(batMode api.BatteryMode) {
	site.batteryMode = batMode
	site.publish(keys.BatteryMode, batMode)
}

// SetBatteryMode sets the battery mode
func (site *Site) SetBatteryMode(batMode api.BatteryMode) {
	site.Lock()
	defer site.Unlock()

	site.log.DEBUG.Println("set battery mode:", batMode)

	if site.batteryMode != batMode {
		site.setBatteryMode(batMode)
	}

	if site.batteryModeExternal == api.BatteryUnknown {
		site.batteryModeExternalTimer = time.Time{}
	}
}

// fromTo reports whether the battery is entering, holding or leaving mode m:
// either m is requested, or nothing is requested and the battery is already in m.
func (site *Site) fromTo(requested, m api.BatteryMode) bool {
	return requested == m || requested == api.BatteryUnknown && site.batteryMode == m
}

func (site *Site) updateBatteryMode(batteryGridChargeActive, batteryGridDischargeActive bool, rate api.Rate) bool {
	batteryMode := site.requiredBatteryMode(batteryGridChargeActive, batteryGridDischargeActive, rate)
	if site.batteryModeHandoffFailed && batteryMode == api.BatteryUnknown {
		batteryMode = api.BatteryNormal
	}

	// put battery into hold mode when charging is active and HEMS dimmed
	if dimmed := hems.Dimmed(site.hems); site.fromTo(batteryMode, api.BatteryCharge) && dimmed != nil && *dimmed {
		site.log.DEBUG.Println("battery mode: HEMS dimmed")
		batteryMode = api.BatteryHold
	}

	// stop discharging to grid when HEMS curtailed production, but keep self-consumption
	if curtailed := hems.Curtailed(site.hems); site.fromTo(batteryMode, api.BatteryDischarge) && curtailed != nil && *curtailed {
		site.log.DEBUG.Println("battery mode: HEMS curtailed")
		batteryMode = api.BatteryNormal
	}

	// NOTE: applyBatteryMode is always called when charge or discharge mode is active to
	// validate max soc / min soc reserve
	if modeChanged := batteryMode != api.BatteryUnknown; modeChanged || site.batteryMode == api.BatteryCharge || site.batteryMode == api.BatteryDischarge {
		if err := site.applyBatteryMode(batteryMode); err == nil {
			if modeChanged {
				site.SetBatteryMode(batteryMode)
			}
		} else {
			site.batteryModeHandoffFailed = true
			site.log.ERROR.Println("battery mode:", err)
			return false
		}
	}

	site.batteryModeHandoffFailed = false
	return true
}

// requiredBatteryMode determines required battery mode based on grid charge/discharge and rate
func (site *Site) requiredBatteryMode(batteryGridChargeActive, batteryGridDischargeActive bool, rate api.Rate) api.BatteryMode {
	var res api.BatteryMode
	batMode := site.GetBatteryMode()
	extMode := site.GetBatteryModeExternal()
	dischargeControl := site.dischargeControlActive(rate)

	var extModeReset bool
	if extMode == api.BatteryUnknown {
		site.Lock()
		extModeReset = !site.batteryModeExternalTimer.IsZero()
		site.Unlock()
	}

	keepUnlessModified := func(s api.BatteryMode) api.BatteryMode {
		return map[bool]api.BatteryMode{false: s, true: api.BatteryUnknown}[batMode == s]
	}

	switch {
	case !site.batteryConfigured():
		res = api.BatteryUnknown
	case extModeReset:
		// require normal mode to leave external control
		res = api.BatteryNormal
	case extMode != api.BatteryUnknown:
		// require external mode only once
		if extMode != batMode {
			res = extMode
		}
	case batteryGridChargeActive:
		// independent limits (buy vs feed-in rate) can both be active at once;
		// charge wins to avoid buying and immediately selling
		if batteryGridDischargeActive && batMode != api.BatteryCharge {
			site.log.WARN.Println("battery mode: grid charge and grid discharge both active, charge takes priority")
		}
		res = keepUnlessModified(api.BatteryCharge)
	case dischargeControl || (batteryGridDischargeActive && site.evFastChargingActive()):
		// hold wins over feed-in discharge; fast charging holds even without
		// batteryDischargeControl, selling while an EV fast-charges is worse
		res = keepUnlessModified(api.BatteryHold)
	case batteryGridDischargeActive:
		res = keepUnlessModified(api.BatteryDischarge)
	case batteryModeModified(batMode):
		res = api.BatteryNormal
	}

	return res
}

// batterySocLimitReached reports whether the battery has reached the soc bound
// that should stop the requested mode: the max soc when charging, or the min
// soc reserve when discharging to grid. A configured limit of 0 disables the
// respective check (max is also disabled at 100).
func (site *Site) batterySocLimitReached(dev config.Device[api.Meter], discharge bool) (bool, error) {
	meter := dev.Instance()

	batLimiter, ok := api.Cap[api.BatterySocLimiter](meter)
	if !ok {
		return false, nil
	}

	batSoc, ok := api.Cap[api.Battery](meter)
	if !ok {
		return false, errors.New("battery with soc limits must have soc")
	}

	soc, err := batSoc.Soc()
	if err != nil {
		return false, err
	}

	minSoc, maxSoc := batLimiter.GetSocLimits()

	if discharge {
		if minSoc > 0 && soc <= minSoc {
			site.log.DEBUG.Printf("battery %s: reserve soc reached (%.0f <= %.0f)", deviceTitleOrName(dev), soc, minSoc)
			return true, nil
		}
		return false, nil
	}

	if maxSoc > 0 && maxSoc < 100 && soc >= maxSoc {
		site.log.DEBUG.Printf("battery %s: limit soc reached (%.0f >= %.0f)", deviceTitleOrName(dev), soc, maxSoc)
		return true, nil
	}

	return false, nil
}

// applyBatteryMode applies the mode to each battery.
//
// A battery that reached the soc bound of the requested mode is held instead:
// the max soc when charging, the min soc reserve when discharging to grid. This
// is decided per device, so one battery reaching its bound does not force the
// others into hold.
func (site *Site) applyBatteryMode(mode api.BatteryMode) error {
	fromToCharge := site.fromTo(mode, api.BatteryCharge)
	fromToDischarge := site.fromTo(mode, api.BatteryDischarge)

	if site.batteryModeApplied == nil {
		site.batteryModeApplied = make(map[string]api.BatteryMode)
	}

	var modeControlled bool
	for _, dev := range site.batteryMeters {
		if api.HasCap[api.BatteryController](dev.Instance()) {
			modeControlled = true
			break
		}
	}

	if mode != api.BatteryUnknown && modeControlled {
		if err := site.stopBatteryPowerControl(); err != nil {
			return err
		}
	}

	for _, dev := range site.batteryMeters {
		meter := dev.Instance()

		batCtrl, ok := api.Cap[api.BatteryController](meter)
		if !ok {
			continue
		}

		// per-device mode so one battery reaching its soc bound does not affect the others
		deviceMode := mode

		// hold at the soc bound of the requested mode (max soc for charge, min soc reserve for grid discharge)
		if (fromToCharge || fromToDischarge) && deviceMode != api.BatteryHold {
			hold, err := site.batterySocLimitReached(dev, fromToDischarge)
			if err != nil && !errors.Is(err, api.ErrNotAvailable) {
				return err
			}
			if hold {
				deviceMode = api.BatteryHold
			}
		}

		// don't re-apply the mode the battery is already in
		name := dev.Config().Name
		if deviceMode == api.BatteryUnknown || deviceMode == site.batteryModeApplied[name] {
			continue
		}

		if !slices.Contains(batCtrl.BatteryModes(), deviceMode) {
			site.log.DEBUG.Printf("battery %s does not support mode: %s", deviceTitleOrName(dev), deviceMode)
			continue
		}

		if err := batCtrl.SetBatteryMode(deviceMode); err != nil {
			if errors.Is(err, api.ErrNotAvailable) {
				continue
			}
			// a partially applied mode makes the cached mode unproven, so Normal
			// recovery must not be skipped later
			delete(site.batteryModeApplied, name)
			return err
		}

		site.batteryModeApplied[name] = deviceMode
		site.log.DEBUG.Printf("set battery %s mode: %s", deviceTitleOrName(dev), deviceMode)
	}

	return nil
}

func (site *Site) tariffRates(usage api.TariffUsage) (api.Rates, error) {
	tariff := site.GetTariff(usage)
	if tariff == nil || tariff.Type() == api.TariffTypePriceStatic {
		return nil, nil
	}

	return tariff.Rates()
}

func (site *Site) smartCostActive(lp loadpoint.API, rate api.Rate) bool {
	limit := lp.GetSmartCostLimit()
	return limit != nil && !rate.IsZero() && rate.Value <= *limit
}

func (site *Site) batteryGridChargeActive(rate api.Rate) bool {
	limit := site.GetBatteryGridChargeLimit()
	return limit != nil && !rate.IsZero() && rate.Value <= *limit
}

// batteryGridDischargeActive reports whether the feed-in rate has reached the
// grid discharge limit; the opt-in gates both this and the optimizer's planning
func (site *Site) batteryGridDischargeActive(rate api.Rate) bool {
	if !site.GetBatteryGridDischarge() {
		return false
	}

	limit := site.GetBatteryGridDischargeLimit()
	return limit != nil && !rate.IsZero() && rate.Value >= *limit
}

func (site *Site) dischargeControlActive(rate api.Rate) bool {
	var fastCharging bool
	for _, lp := range site.activeLoadpoints() {
		smartCostActive := site.smartCostActive(lp, rate)
		if lp.GetStatus() == api.StatusC && (smartCostActive || lp.IsFastChargingActive()) {
			fastCharging = true
			break
		}
	}

	site.Lock()
	defer site.Unlock()

	if !fastCharging {
		site.batteryDischargeHold = false
		return false
	}

	if site.batteryDischargeControl {
		site.batteryDischargeHold = false
		return true
	}

	if site.bufferSoc <= 0 || site.bufferSoc >= 100 {
		site.batteryDischargeHold = false
		return false
	}

	if site.batteryDischargeHold {
		return true
	}

	if !site.batterySocAvailable() {
		site.log.DEBUG.Println("battery discharge hold: battery soc unavailable")
		return true
	}

	if site.battery.Soc <= site.bufferSoc {
		site.batteryDischargeHold = true
		site.log.DEBUG.Printf("battery discharge hold: buffer reached (%.0f%% <= %.0f%%)", site.battery.Soc, site.bufferSoc)
	}

	return site.batteryDischargeHold
}

func (site *Site) batterySocAvailable() bool {
	if len(site.batterySocUpdated) != len(site.batteryMeters) || len(site.batterySocUpdated) == 0 {
		return false
	}
	for _, updated := range site.batterySocUpdated {
		if updated.IsZero() || time.Since(updated) > batteryPowerSocCacheMaxAge {
			return false
		}
	}
	return true
}

// evFastChargingActive reports whether any loadpoint is fast charging,
// regardless of the batteryDischargeControl opt-in
func (site *Site) evFastChargingActive() bool {
	for _, lp := range site.activeLoadpoints() {
		if lp.GetStatus() == api.StatusC && lp.IsFastChargingActive() {
			return true
		}
	}

	return false
}
