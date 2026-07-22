package core

import (
	"testing"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/types"
	"github.com/evcc-io/evcc/tariff"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"go.uber.org/mock/gomock"
)

type testBatteryPowerLimiter struct {
	charge, discharge float64
}

func (l testBatteryPowerLimiter) GetPowerLimits() (float64, float64) {
	return l.charge, l.discharge
}

type testBatterySocLimiter struct {
	min, max float64
}

func (l testBatterySocLimiter) GetSocLimits() (float64, float64) {
	return l.min, l.max
}

func testBatteryPowerSite(
	ctrl *gomock.Controller,
	gridPower, batteryPower, batterySoc float64,
	batteryMode api.BatteryMode,
	powerController api.BatteryPowerController,
	modeController api.BatteryController,
) (*Site, *api.MockMeter, *api.MockMeter, *api.MockBattery) {
	grid := api.NewMockMeter(ctrl)
	batteryMeter := api.NewMockMeter(ctrl)
	battery := api.NewMockBattery(ctrl)

	var bat api.Meter
	if modeController == nil {
		bat = &struct {
			api.Meter
			api.Battery
			api.BatteryPowerController
			api.BatteryPowerLimiter
			api.BatterySocLimiter
		}{
			Meter:                  batteryMeter,
			Battery:                battery,
			BatteryPowerController: powerController,
			BatteryPowerLimiter:    testBatteryPowerLimiter{charge: 5000, discharge: 5000},
			BatterySocLimiter:      testBatterySocLimiter{min: 20, max: 95},
		}
	} else {
		bat = &struct {
			api.Meter
			api.Battery
			api.BatteryController
			api.BatteryPowerController
			api.BatteryPowerLimiter
			api.BatterySocLimiter
		}{
			Meter:                  batteryMeter,
			Battery:                battery,
			BatteryController:      modeController,
			BatteryPowerController: powerController,
			BatteryPowerLimiter:    testBatteryPowerLimiter{charge: 5000, discharge: 5000},
			BatterySocLimiter:      testBatterySocLimiter{min: 20, max: 95},
		}
	}

	site := &Site{
		log:            util.NewLogger("foo"),
		gridMeter:      grid,
		batteryMeters:  []config.Device[api.Meter]{config.NewStaticDevice(config.Named{Name: "bat"}, bat)},
		batteryMode:    batteryMode,
		tariffs:        &tariff.Tariffs{},
		gridPower:      gridPower,
		gridPowerFresh: true,
		batteryPowerFresh: []bool{
			true,
		},
	}
	site.battery.Devices = []types.Measurement{{
		Name:  "bat",
		Power: batteryPower,
		Soc:   &batterySoc,
	}}

	return site, grid, batteryMeter, battery
}

func TestBatteryPowerControlChargeFromSurplus(t *testing.T) {
	ctrl := gomock.NewController(t)
	powerController := api.NewMockBatteryPowerController(ctrl)
	powerController.EXPECT().SetBatteryPower(-5000.0).Times(1)

	site, _, _, _ := testBatteryPowerSite(ctrl, -6000, 0, 50, api.BatteryNormal, powerController, nil)
	site.updateBatteryPowerControl()
}

func TestBatteryPowerControlStopsChargeWhenDimmed(t *testing.T) {
	ctrl := gomock.NewController(t)
	powerController := api.NewMockBatteryPowerController(ctrl)
	powerController.EXPECT().SetBatteryPower(0.0).Times(1)

	site, _, _, _ := testBatteryPowerSite(ctrl, -3000, 0, 50, api.BatteryNormal, powerController, nil)

	maxConsumptionPower := 4200.0
	hems := api.NewMockHEMS(ctrl)
	hems.EXPECT().MaxConsumptionPower().Return(&maxConsumptionPower)
	site.hems = hems

	site.updateBatteryPowerControl()
}

func TestBatteryPowerControlStopsOnStaleGridPower(t *testing.T) {
	ctrl := gomock.NewController(t)
	powerController := api.NewMockBatteryPowerController(ctrl)
	powerController.EXPECT().SetBatteryPower(0.0).Times(1)

	site, _, _, _ := testBatteryPowerSite(ctrl, -3000, 0, 50, api.BatteryNormal, powerController, nil)
	site.gridPowerFresh = false
	site.updateBatteryPowerControl()
}

func TestBatteryPowerControlKeepsExistingChargePower(t *testing.T) {
	ctrl := gomock.NewController(t)
	powerController := api.NewMockBatteryPowerController(ctrl)
	powerController.EXPECT().SetBatteryPower(-4000.0).Times(1)

	site, _, _, _ := testBatteryPowerSite(ctrl, 0, -4000, 50, api.BatteryNormal, powerController, nil)
	site.updateBatteryPowerControl()
}

func TestBatteryPowerControlDischargeOnImport(t *testing.T) {
	ctrl := gomock.NewController(t)
	powerController := api.NewMockBatteryPowerController(ctrl)
	powerController.EXPECT().SetBatteryPower(3000.0).Times(1)

	site, _, _, _ := testBatteryPowerSite(ctrl, 3000, 0, 50, api.BatteryNormal, powerController, nil)
	site.updateBatteryPowerControl()
}

func TestBatteryPowerControlChargeModeIgnoresGridImport(t *testing.T) {
	ctrl := gomock.NewController(t)
	powerController := api.NewMockBatteryPowerController(ctrl)
	powerController.EXPECT().SetBatteryPower(-5000.0).Times(1)

	site, _, _, _ := testBatteryPowerSite(ctrl, 5000, -5000, 50, api.BatteryCharge, powerController, nil)
	site.updateBatteryPowerControl()
}

func TestBatteryPowerControlDefersToModeController(t *testing.T) {
	ctrl := gomock.NewController(t)
	powerController := api.NewMockBatteryPowerController(ctrl)
	modeController := api.NewMockBatteryController(ctrl)

	site, _, _, _ := testBatteryPowerSite(ctrl, 5000, 0, 50, api.BatteryCharge, powerController, modeController)
	site.updateBatteryPowerControl()
}

func TestBatteryPowerControlStopsAtMaxSoc(t *testing.T) {
	ctrl := gomock.NewController(t)
	powerController := api.NewMockBatteryPowerController(ctrl)
	powerController.EXPECT().SetBatteryPower(0.0).Times(1)

	site, _, _, _ := testBatteryPowerSite(ctrl, -3000, 0, 95, api.BatteryNormal, powerController, nil)
	site.updateBatteryPowerControl()
}

func TestBatteryPowerControlFollowsSmallTarget(t *testing.T) {
	ctrl := gomock.NewController(t)
	powerController := api.NewMockBatteryPowerController(ctrl)
	powerController.EXPECT().SetBatteryPower(42.0)

	site, _, _, _ := testBatteryPowerSite(ctrl, 42, 0, 50, api.BatteryNormal, powerController, nil)
	site.updateBatteryPowerControl()
}

func TestBatteryPowerControlStopsOnStaleBatteryPower(t *testing.T) {
	ctrl := gomock.NewController(t)
	powerController := api.NewMockBatteryPowerController(ctrl)
	powerController.EXPECT().SetBatteryPower(0.0)

	site, _, _, _ := testBatteryPowerSite(ctrl, 3000, 0, 50, api.BatteryNormal, powerController, nil)
	site.batteryPowerFresh[0] = false
	site.updateBatteryPowerControl()
}
