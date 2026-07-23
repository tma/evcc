package core

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
)

const (
	batteryPowerControlInterval        = 5 * time.Second
	batteryPowerControlOffset          = batteryPowerControlInterval / 2
	batteryPowerReadTimeout            = 4 * time.Second
	batteryPowerPolicyMaxAge           = 60 * time.Second
	batteryPowerMaxSettleTime          = 30 * time.Second
	batteryPowerCommandRefresh         = 30 * time.Second
	batteryPowerStartDeadband          = 100.0
	batteryPowerActiveDeadband         = 50.0
	batteryPowerGain                   = 0.5
	batteryPowerMaxIncreaseStep        = 1500.0
	batteryPowerWriteThreshold         = 25.0
	batteryPowerAckTolerance           = 250.0
	batteryPowerNeutralTolerance       = 300.0
	batteryPowerAckMovementMinimum     = 10.0
	batteryPowerAckMovementPercentage  = 0.25
	batteryPowerAckTolerancePercentage = 0.5
)

type batteryPowerPhase int

const (
	batteryPowerReleased batteryPowerPhase = iota
	batteryPowerNeutral
	batteryPowerCharging
	batteryPowerDischarging
	batteryPowerFaultStopping
)

func (p batteryPowerPhase) String() string {
	switch p {
	case batteryPowerReleased:
		return "released"
	case batteryPowerNeutral:
		return "neutral"
	case batteryPowerCharging:
		return "charging"
	case batteryPowerDischarging:
		return "discharging"
	case batteryPowerFaultStopping:
		return "fault stopping"
	default:
		return "unknown"
	}
}

type batteryPowerSample struct {
	Value      float64
	StartedAt  time.Time
	FinishedAt time.Time
	Err        error
}

func (s batteryPowerSample) valid(now time.Time) bool {
	return s.validationError(now) == nil
}

func (s batteryPowerSample) validationError(now time.Time) error {
	switch {
	case s.Err != nil:
		return s.Err
	case invalidBatteryPowerValue(s.Value):
		return fmt.Errorf("invalid power value: %v", s.Value)
	case s.StartedAt.IsZero():
		return errors.New("missing read start time")
	case s.FinishedAt.Before(s.StartedAt):
		return errors.New("invalid read timestamps")
	case s.FinishedAt.Sub(s.StartedAt) > batteryPowerReadTimeout:
		return fmt.Errorf("read took %s", s.FinishedAt.Sub(s.StartedAt))
	case now.Sub(s.FinishedAt) > batteryPowerControlInterval:
		return fmt.Errorf("read is %s old", now.Sub(s.FinishedAt))
	default:
		return nil
	}
}

func (s batteryPowerSample) diagnostic() string {
	duration := s.FinishedAt.Sub(s.StartedAt)
	if s.Err != nil {
		return fmt.Sprintf("%v after %s", s.Err, duration)
	}
	return fmt.Sprintf("ok in %s", duration)
}

type pendingBatteryPowerCommand struct {
	PreviousCommand float64
	Command         float64
	BaselinePower   float64
	AppliedAt       time.Time
}

type batteryPowerControlPolicy struct {
	valid            bool
	active           bool
	chargeAllowed    bool
	dischargeAllowed bool
	residualPower    float64
	forceCharge      bool
	chargeLimit      float64
	dischargeLimit   float64
	updatedAt        time.Time
}

type regulatedBattery struct {
	siteIndex  int
	name       string
	meter      api.Meter
	controller api.BatteryPowerController
}

type batteryPowerRegulator struct {
	mu sync.Mutex

	lifecycleMu sync.Mutex
	started     bool
	stopped     bool
	stopC       chan struct{}
	doneC       chan struct{}

	log       *util.Logger
	clock     clock.Clock
	gridMeter api.Meter
	battery   regulatedBattery

	policy            batteryPowerControlPolicy
	phase             batteryPowerPhase
	appliedCommand    float64
	initialized       bool
	pendingCommand    *pendingBatteryPowerCommand
	neutralSince      time.Time
	neutralRequired   bool
	lastBatterySample batteryPowerSample
	lastWriteAt       time.Time
	policyMaxAge      time.Duration
}

func newBatteryPowerRegulator(log *util.Logger, gridMeter api.Meter, devices []config.Device[api.Meter]) *batteryPowerRegulator {
	if gridMeter == nil {
		return nil
	}

	var batteries []regulatedBattery
	for i, dev := range devices {
		ctrl, ok := api.Cap[api.BatteryPowerController](dev.Instance())
		if !ok {
			continue
		}

		batteries = append(batteries, regulatedBattery{
			siteIndex:  i,
			name:       deviceTitleOrName(dev),
			meter:      dev.Instance(),
			controller: ctrl,
		})
	}

	if len(batteries) == 0 {
		return nil
	}
	if len(batteries) > 1 {
		log.ERROR.Printf("battery power control: expected one controller, got %d", len(batteries))
		return nil
	}

	return &batteryPowerRegulator{
		log:          log,
		clock:        clock.New(),
		gridMeter:    gridMeter,
		battery:      batteries[0],
		phase:        batteryPowerReleased,
		policyMaxAge: batteryPowerPolicyMaxAge,
		stopC:        make(chan struct{}),
		doneC:        make(chan struct{}),
	}
}

func (r *batteryPowerRegulator) setSiteInterval(interval time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.policyMaxAge = max(batteryPowerPolicyMaxAge, 2*interval)
}

func (r *batteryPowerRegulator) start() {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()

	if r.started || r.stopped {
		return
	}

	r.started = true
	go r.run()
}

func (r *batteryPowerRegulator) stop() error {
	r.lifecycleMu.Lock()
	if !r.stopped {
		r.stopped = true
		close(r.stopC)
	}
	started := r.started
	doneC := r.doneC
	r.lifecycleMu.Unlock()

	if started {
		err := r.release()
		<-doneC
		return err
	}

	return r.release()
}

func (r *batteryPowerRegulator) run() {
	defer close(r.doneC)

	nextTick := r.clock.Now().Add(batteryPowerControlInterval + batteryPowerControlOffset)
	select {
	case <-r.stopC:
		return
	default:
		r.tick()
	}

	for {
		now := r.clock.Now()
		for !nextTick.After(now) {
			nextTick = nextTick.Add(batteryPowerControlInterval)
		}
		timer := r.clock.Timer(nextTick.Sub(now))

		select {
		case <-timer.C:
			nextTick = nextTick.Add(batteryPowerControlInterval)
			r.tick()
		case <-r.stopC:
			timer.Stop()
			return
		}
	}
}

func (r *batteryPowerRegulator) setPolicy(policy batteryPowerControlPolicy) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	policy.updatedAt = r.clock.Now()
	previous := r.policy
	r.policy = policy

	if !policy.valid || !policy.active {
		return r.releaseLocked("policy released control")
	}

	if r.phase == batteryPowerReleased {
		if err := r.applyCommandLocked(0, true, "acquired control"); err != nil {
			r.markFaultLocked("control acquisition failed", err)
			return err
		}
		r.phase = batteryPowerNeutral
		r.neutralRequired = true
		r.resetControlLocked()
		r.log.DEBUG.Println("battery power control: activated")
		return nil
	}

	if r.appliedCommand != 0 && policyEligibilityChanged(previous, policy, r.appliedCommand) {
		return r.stopToNeutralLocked("policy eligibility changed")
	}

	if !r.directionAllowedLocked(directionForCommand(r.appliedCommand)) {
		return r.stopToNeutralLocked("policy disallows active direction")
	}

	return nil
}

func policyEligibilityChanged(previous, current batteryPowerControlPolicy, command float64) bool {
	if previous.active != current.active {
		return true
	}

	if command < 0 {
		return previous.chargeAllowed != current.chargeAllowed ||
			previous.chargeLimit != current.chargeLimit
	}
	if command > 0 {
		return previous.dischargeAllowed != current.dischargeAllowed ||
			previous.dischargeLimit != current.dischargeLimit
	}

	return false
}

func (r *batteryPowerRegulator) release() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.releaseLocked("released")
}

func (r *batteryPowerRegulator) releaseLocked(reason string) error {
	if r.phase == batteryPowerReleased && r.initialized && r.appliedCommand == 0 {
		return nil
	}

	if !r.initialized || r.appliedCommand != 0 {
		if err := r.applyCommandLocked(0, true, reason); err != nil {
			r.phase = batteryPowerFaultStopping
			return err
		}
	}

	r.phase = batteryPowerReleased
	r.neutralRequired = false
	r.resetControlLocked()
	r.log.DEBUG.Printf("battery power control: %s", reason)
	return nil
}

func (r *batteryPowerRegulator) tick() {
	r.mu.Lock()
	if r.phase == batteryPowerReleased {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	grid := r.readSample(r.gridMeter)
	now := r.clock.Now()
	if !grid.valid(now) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.phase != batteryPowerReleased {
			r.stopAndFaultLocked("grid unavailable", grid.validationError(now))
		}
		return
	}

	battery := r.readSample(r.battery.meter)

	r.mu.Lock()
	defer r.mu.Unlock()

	now = r.clock.Now()
	if r.phase == batteryPowerReleased {
		return
	}
	if !grid.valid(now) {
		err := fmt.Errorf("%w; grid read: %s; battery read: %s",
			grid.validationError(now), grid.diagnostic(), battery.diagnostic())
		r.stopAndFaultLocked("grid stale", err)
		return
	}
	if !r.policyFreshLocked(now) {
		r.stopAndFaultLocked("policy stale", nil)
		return
	}
	if !battery.valid(now) {
		r.stopAndFaultLocked("battery feedback unavailable", battery.validationError(now))
		return
	}

	r.lastBatterySample = battery

	if r.phase == batteryPowerFaultStopping {
		r.rearmFaultLocked(grid, battery)
		return
	}

	if !r.directionAllowedLocked(directionForCommand(r.appliedCommand)) {
		if err := r.stopToNeutralLocked("direction no longer allowed"); err != nil {
			r.markFaultLocked("policy stop failed", err)
		}
		return
	}

	r.updateAcknowledgementLocked(battery)

	if !r.policy.forceCharge {
		target := r.gridTargetLocked(r.phase)
		rawError := grid.Value - target
		if command, ok := immediateBatteryPowerRetreat(r.appliedCommand, rawError); ok {
			if math.Abs(command) < batteryPowerWriteThreshold {
				command = 0
			}
			if command != r.appliedCommand &&
				(command == 0 || math.Abs(command-r.appliedCommand) >= batteryPowerWriteThreshold) {
				if err := r.applyCommandLocked(command, false, "raw grid safety retreat"); err != nil {
					r.markFaultLocked("safety retreat failed", err)
				}
			}
			return
		}
	}

	if r.pendingCommand != nil {
		if r.pendingTimedOutLocked(now) {
			r.stopAndFaultLocked("command acknowledgement timed out", nil)
		}
		return
	}

	if r.policy.forceCharge {
		r.advanceForceChargeLocked(now, battery)
		return
	}

	direction, target, ok := r.controlDemandLocked(grid.Value)
	if !ok {
		r.maybeRefreshCommandLocked(now)
		return
	}

	rawError := grid.Value - target
	if math.Abs(rawError) <= batteryPowerActiveDeadband {
		r.maybeRefreshCommandLocked(now)
		return
	}

	if r.phase == batteryPowerNeutral && !r.observeNeutralLocked(battery) {
		return
	}
	if !r.directionAllowedLocked(direction) {
		return
	}

	command, ok := r.increasedCommandLocked(direction, rawError)
	if !ok {
		r.maybeRefreshCommandLocked(now)
		return
	}

	if err := r.applyCommandLocked(command, false, "acknowledged bounded correction"); err != nil {
		r.markFaultLocked("command failed", err)
	}
}

func (r *batteryPowerRegulator) readSample(meter api.Meter) batteryPowerSample {
	sample := batteryPowerSample{StartedAt: r.clock.Now()}
	sample.Value, sample.Err = meter.CurrentPower()
	sample.FinishedAt = r.clock.Now()
	return sample
}

func (r *batteryPowerRegulator) policyFreshLocked(now time.Time) bool {
	return r.policy.valid &&
		r.policy.active &&
		!invalidBatteryPowerValue(r.policy.residualPower) &&
		!r.policy.updatedAt.IsZero() &&
		now.Sub(r.policy.updatedAt) <= r.policyMaxAge
}

func (r *batteryPowerRegulator) directionAllowedLocked(direction batteryPowerPhase) bool {
	switch direction {
	case batteryPowerCharging:
		return r.policy.chargeAllowed && r.policy.chargeLimit > 0
	case batteryPowerDischarging:
		return r.policy.dischargeAllowed && r.policy.dischargeLimit > 0
	default:
		return true
	}
}

func (r *batteryPowerRegulator) gridTargetLocked(direction batteryPowerPhase) float64 {
	if direction == batteryPowerCharging {
		return -r.policy.residualPower
	}
	return 0
}

func (r *batteryPowerRegulator) controlDemandLocked(gridPower float64) (batteryPowerPhase, float64, bool) {
	switch r.phase {
	case batteryPowerCharging:
		return batteryPowerCharging, r.gridTargetLocked(batteryPowerCharging), true
	case batteryPowerDischarging:
		return batteryPowerDischarging, 0, true
	case batteryPowerNeutral:
		chargeTarget := -r.policy.residualPower
		chargeStartTarget := min(chargeTarget, 0)

		switch {
		case r.directionAllowedLocked(batteryPowerCharging) && gridPower < chargeStartTarget-batteryPowerStartDeadband:
			return batteryPowerCharging, chargeTarget, true
		case r.directionAllowedLocked(batteryPowerDischarging) && gridPower > batteryPowerStartDeadband:
			return batteryPowerDischarging, 0, true
		}
	}

	return batteryPowerNeutral, 0, false
}

func immediateBatteryPowerRetreat(command, rawError float64) (float64, bool) {
	switch {
	case command < 0 && rawError > batteryPowerActiveDeadband:
		return math.Min(0, command+rawError), true
	case command > 0 && rawError < -batteryPowerActiveDeadband:
		return math.Max(0, command+rawError), true
	default:
		return command, false
	}
}

func (r *batteryPowerRegulator) increasedCommandLocked(direction batteryPowerPhase, rawError float64) (float64, bool) {
	var delta float64
	switch direction {
	case batteryPowerCharging:
		if rawError >= -batteryPowerActiveDeadband {
			return 0, false
		}
		delta = max(rawError*batteryPowerGain, -batteryPowerMaxIncreaseStep)
	case batteryPowerDischarging:
		if rawError <= batteryPowerActiveDeadband {
			return 0, false
		}
		delta = min(rawError*batteryPowerGain, batteryPowerMaxIncreaseStep)
	default:
		return 0, false
	}

	command := math.Round(r.appliedCommand + delta)
	command = min(r.policy.dischargeLimit, max(-r.policy.chargeLimit, command))
	if direction == batteryPowerCharging {
		command = min(0, command)
	} else {
		command = max(0, command)
	}

	if math.Abs(command) <= math.Abs(r.appliedCommand) ||
		math.Abs(command-r.appliedCommand) < batteryPowerWriteThreshold {
		return 0, false
	}

	return command, true
}

func (r *batteryPowerRegulator) observeNeutralLocked(sample batteryPowerSample) bool {
	if !r.neutralRequired {
		return true
	}
	if !sample.StartedAt.After(r.neutralSince) ||
		math.Abs(sample.Value) > batteryPowerNeutralTolerance {
		return false
	}

	r.neutralRequired = false
	return true
}

func (r *batteryPowerRegulator) updateAcknowledgementLocked(sample batteryPowerSample) {
	pending := r.pendingCommand
	if pending == nil || !sample.StartedAt.After(pending.AppliedAt) {
		return
	}

	delta := pending.Command - pending.PreviousCommand
	tolerance := min(batteryPowerAckTolerance, math.Abs(delta)*batteryPowerAckTolerancePercentage)

	if math.Abs(pending.Command) < math.Abs(pending.PreviousCommand) {
		switch {
		case pending.Command < 0 && sample.Value >= pending.Command-tolerance:
			r.pendingCommand = nil
		case pending.Command > 0 && sample.Value <= pending.Command+tolerance:
			r.pendingCommand = nil
		}
		return
	}

	switch {
	case delta < 0 && sample.Value <= pending.Command+tolerance:
		r.pendingCommand = nil
		return
	case delta > 0 && sample.Value >= pending.Command-tolerance:
		r.pendingCommand = nil
		return
	}

	movement := sample.Value - pending.BaselinePower
	required := max(batteryPowerAckMovementMinimum, math.Abs(delta)*batteryPowerAckMovementPercentage)
	if delta < 0 && movement <= -required || delta > 0 && movement >= required {
		r.pendingCommand = nil
	}
}

func (r *batteryPowerRegulator) pendingTimedOutLocked(now time.Time) bool {
	return r.pendingCommand != nil &&
		now.Sub(r.pendingCommand.AppliedAt) >= batteryPowerMaxSettleTime
}

func (r *batteryPowerRegulator) advanceForceChargeLocked(now time.Time, battery batteryPowerSample) {
	if r.phase == batteryPowerDischarging {
		if err := r.stopToNeutralLocked("force charge direction change"); err != nil {
			r.markFaultLocked("force charge stop failed", err)
		}
		return
	}
	if r.phase == batteryPowerNeutral && !r.observeNeutralLocked(battery) {
		return
	}
	if !r.directionAllowedLocked(batteryPowerCharging) {
		if err := r.stopToNeutralLocked("force charge unavailable"); err != nil {
			r.markFaultLocked("force charge stop failed", err)
		}
		return
	}

	command := max(-r.policy.chargeLimit, r.appliedCommand-batteryPowerMaxIncreaseStep)
	if math.Abs(command-r.appliedCommand) < batteryPowerWriteThreshold {
		r.maybeRefreshCommandLocked(now)
		return
	}

	if err := r.applyCommandLocked(command, false, "forced charge"); err != nil {
		r.markFaultLocked("force charge command failed", err)
	}
}

func (r *batteryPowerRegulator) maybeRefreshCommandLocked(now time.Time) {
	if r.appliedCommand == 0 || now.Sub(r.lastWriteAt) < batteryPowerCommandRefresh {
		return
	}

	if err := r.applyCommandLocked(r.appliedCommand, true, "forced-control refresh"); err != nil {
		r.markFaultLocked("command refresh failed", err)
	}
}

func (r *batteryPowerRegulator) applyCommandLocked(command float64, force bool, reason string) error {
	command = math.Round(command)
	if !force && command != 0 && math.Abs(command-r.appliedCommand) < batteryPowerWriteThreshold {
		return nil
	}
	if command == r.appliedCommand && r.initialized && !force {
		return nil
	}

	previous := r.appliedCommand
	baseline := r.lastBatterySample.Value
	if err := r.battery.controller.SetBatteryPower(command); err != nil {
		stopErr := r.battery.controller.SetBatteryPower(0)
		if stopErr == nil {
			r.appliedCommand = 0
			r.initialized = true
			r.lastWriteAt = r.clock.Now()
		}
		r.pendingCommand = nil
		r.phase = batteryPowerFaultStopping
		return errors.Join(fmt.Errorf("%s: %w", r.battery.name, err), stopErr)
	}

	now := r.clock.Now()
	r.appliedCommand = command
	r.initialized = true
	r.lastWriteAt = now

	switch {
	case command == 0:
		r.phase = batteryPowerNeutral
		r.neutralSince = now
		r.neutralRequired = true
		r.resetControlLocked()
	case command < 0:
		r.phase = batteryPowerCharging
	case command > 0:
		r.phase = batteryPowerDischarging
	}

	if command != 0 && command != previous {
		r.pendingCommand = &pendingBatteryPowerCommand{
			PreviousCommand: previous,
			Command:         command,
			BaselinePower:   baseline,
			AppliedAt:       now,
		}
	} else {
		r.pendingCommand = nil
	}

	r.log.DEBUG.Printf(
		"battery power control: phase=%s command=%.0fW grid-target=%.0fW reason=%s",
		r.phase, command, r.gridTargetLocked(r.phase), reason,
	)
	return nil
}

func (r *batteryPowerRegulator) stopToNeutralLocked(reason string) error {
	if r.appliedCommand == 0 && r.initialized {
		r.phase = batteryPowerNeutral
		return nil
	}
	return r.applyCommandLocked(0, true, reason)
}

func (r *batteryPowerRegulator) stopAndFaultLocked(reason string, cause error) {
	if r.phase == batteryPowerFaultStopping && r.appliedCommand == 0 && r.initialized {
		return
	}

	err := r.applyCommandLocked(0, true, reason)
	r.markFaultLocked(reason, errors.Join(cause, err))
}

func (r *batteryPowerRegulator) markFaultLocked(reason string, err error) {
	r.phase = batteryPowerFaultStopping
	if err != nil {
		r.log.ERROR.Printf("battery power control: %s: %v", reason, err)
	} else {
		r.log.ERROR.Printf("battery power control: %s", reason)
	}
}

func (r *batteryPowerRegulator) rearmFaultLocked(grid, battery batteryPowerSample) {
	if !r.initialized || r.appliedCommand != 0 {
		if err := r.applyCommandLocked(0, true, "fault stop retry"); err != nil {
			r.log.ERROR.Printf("battery power control: stop retry: %v", err)
		}
		return
	}
	if !battery.StartedAt.After(r.lastWriteAt) ||
		math.Abs(battery.Value) > batteryPowerNeutralTolerance {
		return
	}

	r.phase = batteryPowerNeutral
	r.neutralRequired = false
	r.resetControlLocked()
	r.log.DEBUG.Printf("battery power control: rearmed at grid %.0fW", grid.Value)
}

func (r *batteryPowerRegulator) resetControlLocked() {
	r.pendingCommand = nil
}

func directionForCommand(command float64) batteryPowerPhase {
	switch {
	case command < 0:
		return batteryPowerCharging
	case command > 0:
		return batteryPowerDischarging
	default:
		return batteryPowerNeutral
	}
}
