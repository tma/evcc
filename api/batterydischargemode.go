package api

import "fmt"

// BatteryDischargeMode controls home battery support during fast and planned charging.
type BatteryDischargeMode string

const (
	BatteryDischargeAllow   BatteryDischargeMode = "allow"
	BatteryDischargeReserve BatteryDischargeMode = "reserve"
	BatteryDischargePrevent BatteryDischargeMode = "prevent"
)

// String implements Stringer.
func (mode BatteryDischargeMode) String() string {
	return string(mode)
}

// BatteryDischargeModeString parses a battery discharge mode.
func BatteryDischargeModeString(value string) (BatteryDischargeMode, error) {
	mode := BatteryDischargeMode(value)
	switch mode {
	case BatteryDischargeAllow, BatteryDischargeReserve, BatteryDischargePrevent:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid battery discharge mode: %s", value)
	}
}
