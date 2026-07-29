package meter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/plugin"
	"github.com/evcc-io/evcc/util"
)

type batteryCapacity struct {
	Capacity float64
}

// var _ api.BatteryCapacity = (*batteryCapacity)(nil)

// Decorator returns an api.BatteryCapacity decorator
func (m *batteryCapacity) Decorator() func() float64 {
	if m.Capacity == 0 {
		return nil
	}
	return func() float64 {
		return m.Capacity
	}
}

type batteryCapacityCtx struct {
	Capacity any // static kWh value or float plugin
}

// var _ api.BatteryCapacity = (*batteryCapacityCtx)(nil)

// Decorator returns an api.BatteryCapacity decorator. Capacity may be a static
// number or a float plugin config; nil/zero means not configured.
func (m *batteryCapacityCtx) Decorator(ctx context.Context) (func() float64, error) {
	return resolveFloat(ctx, m.Capacity)
}

// resolveFloat resolves a static number or float plugin config to a getter.
// nil/zero static returns a nil getter (not configured).
func resolveFloat(ctx context.Context, v any) (func() float64, error) {
	switch v := v.(type) {
	case nil:
		return nil, nil
	case int:
		return staticCapacity(float64(v)), nil
	case int64:
		return staticCapacity(float64(v)), nil
	case float64:
		return staticCapacity(v), nil
	default:
		var cfg plugin.Config
		if err := util.DecodeOther(v, &cfg); err != nil {
			return nil, err
		}
		get, err := cfg.FloatGetter(ctx)
		if err != nil {
			return nil, err
		}
		return func() float64 {
			f, err := get()
			if err != nil {
				return 0 // ponytail: treat plugin error as unknown value
			}
			return f
		}, nil
	}
}

func staticCapacity(f float64) func() float64 {
	if f == 0 {
		return nil
	}
	return func() float64 { return f }
}

// floatOr0 evaluates g, returning 0 for a nil (unconfigured) getter.
func floatOr0(g func() float64) float64 {
	if g == nil {
		return 0
	}
	return g()
}

type batteryPowerControlConfig struct {
	Charge          *plugin.Config
	ChargeUpdate    *plugin.Config
	Discharge       *plugin.Config
	DischargeUpdate *plugin.Config
	Stop            *plugin.Config
	Refresh         time.Duration
}

type batteryPowerController struct {
	mu              sync.Mutex
	charge          func(float64) error
	chargeUpdate    func(float64) error
	discharge       func(float64) error
	dischargeUpdate func(float64) error
	stop            func(float64) error
	refresh         time.Duration
	now             func() time.Time
	lastFullWrite   time.Time
	direction       int
	initialized     bool
}

func (cc *batteryPowerControlConfig) Controller(ctx context.Context) (func(float64) error, error) {
	if cc == nil {
		return nil, nil
	}

	charge, err := cc.Charge.IntSetter(ctx, "power")
	if err != nil {
		return nil, fmt.Errorf("battery power charge: %w", err)
	}

	discharge, err := cc.Discharge.IntSetter(ctx, "power")
	if err != nil {
		return nil, fmt.Errorf("battery power discharge: %w", err)
	}

	chargeUpdate, err := cc.ChargeUpdate.IntSetter(ctx, "power")
	if err != nil {
		return nil, fmt.Errorf("battery power charge update: %w", err)
	}

	dischargeUpdate, err := cc.DischargeUpdate.IntSetter(ctx, "power")
	if err != nil {
		return nil, fmt.Errorf("battery power discharge update: %w", err)
	}

	stop, err := cc.Stop.IntSetter(ctx, "power")
	if err != nil {
		return nil, fmt.Errorf("battery power stop: %w", err)
	}

	if charge == nil && discharge == nil && stop == nil {
		return nil, nil
	}

	ctrl := &batteryPowerController{
		charge:          batteryPowerSetter(charge),
		chargeUpdate:    batteryPowerSetter(chargeUpdate),
		discharge:       batteryPowerSetter(discharge),
		dischargeUpdate: batteryPowerSetter(dischargeUpdate),
		stop:            batteryPowerSetter(stop),
		refresh:         cc.Refresh,
		now:             time.Now,
	}

	return ctrl.SetBatteryPower, nil
}

func batteryPowerSetter(set func(int64) error) func(float64) error {
	if set == nil {
		return nil
	}
	return func(power float64) error {
		return set(int64(math.Round(power)))
	}
}

func (c *batteryPowerController) currentTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *batteryPowerController) setDirectionalPower(direction int, power float64, full, update func(float64) error) error {
	set := full
	fullWrite := true

	if c.direction == direction && update != nil && c.refresh > 0 && !c.lastFullWrite.IsZero() {
		if elapsed := c.currentTime().Sub(c.lastFullWrite); elapsed >= 0 && elapsed < c.refresh {
			set = update
			fullWrite = false
		}
	}

	if err := set(power); err != nil {
		c.direction = 0
		c.initialized = false
		return err
	}
	if fullWrite {
		c.lastFullWrite = c.currentTime()
	}

	c.direction = direction
	c.initialized = true
	return nil
}

// SetBatteryPower sets battery power and resets the previous direction's watchdog.
func (c *batteryPowerController) SetBatteryPower(power float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch {
	case power < 0:
		if c.charge == nil {
			return api.ErrNotAvailable
		}
		if c.direction > 0 {
			if err := c.discharge(0); err != nil {
				return err
			}
		}
		if err := c.setDirectionalPower(-1, -power, c.charge, c.chargeUpdate); err != nil {
			return err
		}

	case power > 0:
		if c.discharge == nil {
			return api.ErrNotAvailable
		}
		if c.direction < 0 {
			if err := c.charge(0); err != nil {
				return err
			}
		}
		if err := c.setDirectionalPower(1, power, c.discharge, c.dischargeUpdate); err != nil {
			return err
		}

	default:
		var directionErr error
		switch c.direction {
		case -1:
			directionErr = c.charge(0)
		case 1:
			directionErr = c.discharge(0)
		default:
			if !c.initialized && c.stop == nil {
				if c.charge != nil {
					directionErr = errors.Join(directionErr, c.charge(0))
				}
				if c.discharge != nil {
					directionErr = errors.Join(directionErr, c.discharge(0))
				}
			}
		}
		var stopErr error
		if c.stop != nil {
			stopErr = c.stop(0)
		}
		if err := errors.Join(directionErr, stopErr); err != nil {
			return err
		}
		c.direction = 0
		c.initialized = true
		c.lastFullWrite = time.Time{}
	}

	return nil
}

type batteryPowerLimits struct {
	MaxChargePower    float64
	MaxDischargePower float64
}

// var _ api.BatteryPowerLimiter = (*batteryPowerLimits)(nil)

// Decorator returns an api.BatteryPowerLimiter decorator
func (m *batteryPowerLimits) Decorator() func() (float64, float64) {
	if m.MaxChargePower == 0 || m.MaxDischargePower == 0 {
		return nil
	}
	return func() (float64, float64) {
		return m.MaxChargePower, m.MaxDischargePower
	}
}

type batteryPowerLimitsCtx struct {
	MaxChargePower    any // static W value or float plugin
	MaxDischargePower any // static W value or float plugin
}

// var _ api.BatteryPowerLimiter = (*batteryPowerLimitsCtx)(nil)

// Decorator returns an api.BatteryPowerLimiter decorator. Each limit may be a
// static number or a float plugin config; either unset means not configured.
func (m *batteryPowerLimitsCtx) Decorator(ctx context.Context) (func() (float64, float64), error) {
	charge, err := resolveFloat(ctx, m.MaxChargePower)
	if err != nil {
		return nil, err
	}
	discharge, err := resolveFloat(ctx, m.MaxDischargePower)
	if err != nil {
		return nil, err
	}
	if charge == nil || discharge == nil {
		return nil, nil
	}
	return func() (float64, float64) {
		return charge(), discharge()
	}, nil
}

type batterySocLimits struct {
	MinSoc, MaxSoc float64
}

// var _ api.BatterySocLimiter = (*batterySocLimits)(nil)

// Decorator returns an api.BatterySocLimiter decorator
func (m *batterySocLimits) Decorator() func() (float64, float64) {
	if m.MinSoc == 0 && m.MaxSoc == 0 {
		return nil
	}
	return func() (float64, float64) {
		return m.MinSoc, m.MaxSoc
	}
}

// LimitController returns an api.BatteryController decorator
func (m *batterySocLimits) LimitController(socG func() (float64, error), limitSocS func(float64) error) func(api.BatteryMode) error {
	return func(mode api.BatteryMode) error {
		switch mode {
		case api.BatteryNormal:
			return limitSocS(m.MinSoc)

		case api.BatteryHold:
			soc, err := socG()
			if err != nil {
				return err
			}
			return limitSocS(min(100, max(soc, m.MinSoc)))

		case api.BatteryCharge:
			return limitSocS(m.MaxSoc)

		// BatteryHoldCharge implementable via limit soc
		default:
			return api.ErrNotAvailable
		}
	}
}

type batterySocLimitsCtx struct {
	MinSoc, MaxSoc any // static % value or float plugin
}

// var _ api.BatterySocLimiter = (*batterySocLimitsCtx)(nil)

func (m *batterySocLimitsCtx) getters(ctx context.Context) (func() float64, func() float64, error) {
	minG, err := resolveFloat(ctx, m.MinSoc)
	if err != nil {
		return nil, nil, err
	}
	maxG, err := resolveFloat(ctx, m.MaxSoc)
	if err != nil {
		return nil, nil, err
	}
	return minG, maxG, nil
}

// Decorator returns an api.BatterySocLimiter decorator. Each limit may be a
// static number or a float plugin config; both unset means not configured.
func (m *batterySocLimitsCtx) Decorator(ctx context.Context) (func() (float64, float64), error) {
	minG, maxG, err := m.getters(ctx)
	if err != nil {
		return nil, err
	}
	if minG == nil && maxG == nil {
		return nil, nil
	}
	return func() (float64, float64) {
		return floatOr0(minG), floatOr0(maxG)
	}, nil
}

// LimitController returns an api.BatteryController decorator
func (m *batterySocLimitsCtx) LimitController(ctx context.Context, socG func() (float64, error), limitSocS func(float64) error) (func(api.BatteryMode) error, error) {
	minG, maxG, err := m.getters(ctx)
	if err != nil {
		return nil, err
	}
	return func(mode api.BatteryMode) error {
		switch mode {
		case api.BatteryNormal:
			return limitSocS(floatOr0(minG))

		case api.BatteryHold:
			soc, err := socG()
			if err != nil {
				return err
			}
			return limitSocS(min(100, max(soc, floatOr0(minG))))

		case api.BatteryCharge:
			return limitSocS(floatOr0(maxG))

		// BatteryHoldCharge not implementable via limit soc
		default:
			return api.ErrNotAvailable
		}
	}, nil
}
