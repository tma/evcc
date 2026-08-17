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

var errBatteryPowerStopRetryPending = errors.New("battery power stop retry pending")

const (
	batteryPowerControlInterval        = 3 * time.Second
	batteryPowerControlOffset          = batteryPowerControlInterval / 2
	batteryPowerGridReadTimeout        = 4 * time.Second
	batteryPowerFeedbackGrace          = 15 * time.Second
	batteryPowerPolicyMaxAge           = 60 * time.Second
	batteryPowerMaxSettleTime          = 30 * time.Second
	batteryPowerFirstCooldown          = time.Minute
	batteryPowerRepeatedCooldown       = 10 * time.Minute
	batteryPowerCommandRefresh         = 30 * time.Second
	batteryPowerStopRetrySafetyWindow  = time.Minute
	batteryPowerStopRetryInterval      = time.Minute
	batteryPowerStartDeadband          = 50.0
	batteryPowerActiveDeadband         = 50.0
	batteryPowerDischargeGridTarget    = -50.0
	batteryPowerGain                   = 0.67 // Retains margin for partially applied commands.
	batteryPowerMaxIncreaseStep        = 1500.0
	batteryPowerFastImportThreshold    = 500.0
	batteryPowerFastImportMaxGap       = batteryPowerControlInterval + batteryPowerControlInterval/2
	batteryPowerFastDischargeGain      = 1.0
	batteryPowerFastDischargeFirstStep = 2000.0
	batteryPowerFastDischargeMaxStep   = 4000.0
	batteryPowerWriteThreshold         = 25.0
	batteryPowerAckTolerance           = 250.0
	batteryPowerNeutralTolerance       = 300.0
	batteryPowerAckMovementMinimum     = 10.0
	batteryPowerAckMovementPercentage  = 0.25
	batteryPowerAckTolerancePercentage = 0.5
	batteryPowerMaterialFloor          = 250.0
	batteryPowerMaterialPercentage     = 0.10
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

type batteryPowerObservationSample struct {
	Power      float64
	FinishedAt time.Time
	Valid      bool
}

type batteryPowerObservation struct {
	Grid         batteryPowerObservationSample
	Battery      batteryPowerObservationSample
	BatteryIndex int
}

type batteryPowerSampleObserver func(batteryPowerObservation)

func (s batteryPowerSample) valid(now time.Time, readTimeout time.Duration) bool {
	return s.validationError(now, readTimeout) == nil
}

func (s batteryPowerSample) validationError(now time.Time, readTimeout time.Duration) error {
	switch {
	case s.Err != nil:
		return s.Err
	case invalidBatteryPowerValue(s.Value):
		return fmt.Errorf("invalid power value: %v", s.Value)
	case s.StartedAt.IsZero():
		return errors.New("missing read start time")
	case s.FinishedAt.Before(s.StartedAt):
		return errors.New("invalid read timestamps")
	case readTimeout > 0 && s.FinishedAt.Sub(s.StartedAt) > readTimeout:
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
	soc              float64
	minSoc           float64
	maxSoc           float64
	socLimitsValid   bool
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
	lastCommandReason string
	lastFastImportAt  time.Time
	stopFailureSince  time.Time
	lastStopAttemptAt time.Time
	policyMaxAge      time.Duration
	diagnosticCycle   uint64

	chargeBlockedUntil    time.Time
	dischargeBlockedUntil time.Time

	sampleObserverMu         sync.RWMutex // Release barrier for callbacks, which run without r.mu.
	sampleObserver           batteryPowerSampleObserver
	sampleObserverGeneration uint64
	sampleObserverEnabled    bool
	sampleObserverRecover    bool
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

func (r *batteryPowerRegulator) setSampleObserver(observer batteryPowerSampleObserver) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.sampleObserver = observer
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
		return r.releaseLocked("policy released control", false)
	}

	if r.phase == batteryPowerReleased {
		if err := r.applyCommandLocked(0, true, "acquired control"); err != nil {
			r.sampleObserverRecover = true
			r.markFaultLocked("control acquisition failed", err)
			return err
		}
		r.phase = batteryPowerNeutral
		r.neutralRequired = true
		r.resetControlLocked()
		r.sampleObserverEnabled = true
		r.sampleObserverRecover = false
		r.log.DEBUG.Println("battery power control: activated")
		return nil
	}

	if r.appliedCommand != 0 && policyEligibilityChanged(previous, policy, r.appliedCommand) {
		return r.stopToNeutralLocked("policy eligibility changed")
	}

	if !r.directionAllowedLocked(directionForCommand(r.appliedCommand)) {
		return r.stopToNeutralLocked("policy disallows active direction")
	}

	// A tighter same-direction cap must retreat, not idle through zero.
	return r.clampAppliedCommandToPolicyLocked()
}

func policyEligibilityChanged(previous, current batteryPowerControlPolicy, command float64) bool {
	if previous.active != current.active {
		return true
	}

	if command < 0 {
		return previous.chargeAllowed != current.chargeAllowed
	}
	if command > 0 {
		return previous.dischargeAllowed != current.dischargeAllowed
	}

	return false
}

func (r *batteryPowerRegulator) clampAppliedCommandToPolicyLocked() error {
	command := r.appliedCommand
	switch {
	case command < 0:
		command = max(-r.policy.chargeLimit, command)
	case command > 0:
		command = min(r.policy.dischargeLimit, command)
	default:
		return nil
	}

	if math.Abs(command) < batteryPowerWriteThreshold {
		command = 0
	}
	if command == r.appliedCommand ||
		command != 0 && math.Abs(command-r.appliedCommand) < batteryPowerWriteThreshold {
		return nil
	}

	return r.applyCommandLocked(command, false, "policy power limit clamp")
}

func (r *batteryPowerRegulator) release() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.releaseLocked("released", true)
}

func (r *batteryPowerRegulator) releaseForHandoff() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.phase == batteryPowerFaultStopping && !r.stopRetryDueLocked(r.clock.Now()) {
		return errBatteryPowerStopRetryPending
	}
	return r.releaseLocked("released for mode handoff", false)
}

func (r *batteryPowerRegulator) releaseLocked(reason string, force bool) error {
	r.sampleObserverEnabled = false
	r.sampleObserverRecover = false
	r.sampleObserverMu.Lock()
	r.sampleObserverGeneration++
	r.sampleObserverMu.Unlock()

	if r.phase == batteryPowerReleased && r.knownStoppedLocked() {
		return nil
	}
	if !force && r.phase == batteryPowerFaultStopping && !r.stopRetryDueLocked(r.clock.Now()) {
		return nil
	}

	if !r.knownStoppedLocked() {
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

	battery := r.readSample(r.battery.meter)
	grid := r.readSample(r.gridMeter)

	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.clock.Now()
	if r.phase == batteryPowerReleased {
		return
	}
	r.diagnosticCycle++
	cycle := r.diagnosticCycle
	gridErr := grid.validationError(now, batteryPowerGridReadTimeout)
	batteryErr := battery.validationError(now, 0)
	if gridErr == nil && batteryErr == nil {
		r.logCycleLocked(cycle, now, grid, battery)
	}
	r.notifySampleObserverLocked(now, grid, battery)
	if gridErr != nil {
		err := fmt.Errorf("%w; grid read: %s; battery read: %s",
			gridErr, grid.diagnostic(), battery.diagnostic())
		r.stopAndFaultLocked("grid unavailable", err)
		return
	}
	if !r.policyFreshLocked(now) {
		r.stopAndFaultLocked("policy stale", nil)
		return
	}

	if !r.directionAllowedLocked(directionForCommand(r.appliedCommand)) {
		if err := r.stopToNeutralLocked("direction no longer allowed"); err != nil {
			r.markFaultLocked("policy stop failed", err)
		}
		return
	}

	if batteryErr != nil {
		err := r.batteryFeedbackErrorLocked(now, battery, batteryErr)
		if r.phase == batteryPowerNeutral && r.initialized && r.appliedCommand == 0 {
			r.markFaultLocked("battery feedback unavailable", err)
		} else if !r.handleUnavailableBatteryFeedbackLocked(now, grid, err) {
			r.stopAndFaultLocked("battery feedback unavailable", err)
		}
		return
	}

	r.lastBatterySample = battery
	fastImportConfirmed := r.sustainedFastImportLocked(grid.FinishedAt, grid.Value)

	if r.phase == batteryPowerFaultStopping {
		r.rearmFaultLocked(grid, battery)
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
				reason := fmt.Sprintf("raw grid safety retreat cycle=%d", cycle)
				if err := r.applyCommandLocked(command, false, reason); err != nil {
					r.markFaultLocked("safety retreat failed", err)
				}
			}
			return
		}
	}

	if r.pendingCommand != nil {
		if !r.policy.forceCharge && r.overridePendingDischargeReductionLocked(grid.Value, fastImportConfirmed) {
			return
		}
		if r.pendingTimedOutLocked(now) {
			if !r.policy.forceCharge && r.rollbackUndemandedIncreaseLocked(grid.Value) {
				return
			}
			if r.chargingSaturationHoldLocked(now, grid, battery) {
				return
			}
			r.stopForCommandTimeoutLocked(now, grid, battery)
		}
		return
	}

	// With no pending command in flight to catch it via timeout, established
	// charging that has turned materially wrong-direction must hard-stop
	// immediately rather than sit silently blocked by the anti-windup gate:
	// otherwise a charging command could remain applied forever while the
	// battery is measurably discharging.
	if r.appliedCommand < 0 && battery.Value > batteryPowerNeutralTolerance {
		r.stopForChargingWrongDirectionLocked(now, grid, battery)
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

	command, ok := r.increasedCommandLocked(direction, grid.Value, rawError, fastImportConfirmed)
	if !ok {
		r.maybeRefreshCommandLocked(now)
		return
	}

	if err := r.applyCommandLocked(command, false, "acknowledged bounded correction"); err != nil {
		r.markFaultLocked("command failed", err)
	}
}

func (r *batteryPowerRegulator) logCycleLocked(cycle uint64, now time.Time, grid, battery batteryPowerSample) {
	direction, target, demanded := r.controlDemandLocked(grid.Value)
	demand := "none"
	if demanded {
		demand = direction.String()
	}

	pending := "none"
	if command := r.pendingCommand; command != nil {
		pending = fmt.Sprintf(
			"command:%.0fW,previous:%.0fW,baseline:%.0fW,age:%s,sample-after:%t",
			command.Command, command.PreviousCommand, command.BaselinePower,
			now.Sub(command.AppliedAt).Round(time.Millisecond),
			battery.StartedAt.After(command.AppliedAt),
		)
	}

	neutralObserved := r.neutralRequired && r.neutralSampleObservedLocked(battery)
	neutralAge := "none"
	if r.neutralRequired {
		neutralAge = batteryPowerDiagnosticAge(now, r.neutralSince)
	}

	r.log.DEBUG.Printf(
		"battery power control: cycle=%d phase=%s grid=%.0fW battery=%.0fW command=%.0fW pending=%s demand=%s target=%.0fW error=%.0fW charge-available=%t discharge-available=%t force-charge=%t policy-age=%s initialized=%t neutral-required=%t neutral-observed=%t neutral-age=%s write-age=%s last-action=%q stop-failure-age=%s stop-attempt-age=%s battery-read=%s grid-read=%s battery-age=%s grid-age=%s",
		cycle, r.phase, grid.Value, battery.Value, r.appliedCommand, pending, demand, target, grid.Value-target,
		r.directionAllowedLocked(batteryPowerCharging), r.directionAllowedLocked(batteryPowerDischarging),
		r.policy.forceCharge, batteryPowerDiagnosticAge(now, r.policy.updatedAt), r.initialized,
		r.neutralRequired, neutralObserved,
		neutralAge, batteryPowerDiagnosticAge(now, r.lastWriteAt),
		r.lastCommandReason,
		batteryPowerDiagnosticAge(now, r.stopFailureSince),
		batteryPowerDiagnosticAge(now, r.lastStopAttemptAt),
		battery.FinishedAt.Sub(battery.StartedAt), grid.FinishedAt.Sub(grid.StartedAt),
		now.Sub(battery.FinishedAt), now.Sub(grid.FinishedAt),
	)
}

func batteryPowerDiagnosticAge(now, event time.Time) string {
	if event.IsZero() {
		return "none"
	}
	return now.Sub(event).Round(time.Millisecond).String()
}

func (r *batteryPowerRegulator) notifySampleObserverLocked(now time.Time, grid, battery batteryPowerSample) {
	if r.sampleObserver == nil || !r.sampleObserverEnabled {
		return
	}

	observation := batteryPowerObservation{BatteryIndex: r.battery.siteIndex}
	if grid.valid(now, batteryPowerGridReadTimeout) {
		observation.Grid = batteryPowerObservationSample{
			Power:      grid.Value,
			FinishedAt: grid.FinishedAt,
			Valid:      true,
		}
	}
	if battery.valid(now, 0) {
		observation.Battery = batteryPowerObservationSample{
			Power:      battery.Value,
			FinishedAt: battery.FinishedAt,
			Valid:      true,
		}
	}
	if !observation.Grid.Valid && !observation.Battery.Valid {
		return
	}

	observer := r.sampleObserver
	r.sampleObserverMu.RLock()
	generation := r.sampleObserverGeneration
	r.sampleObserverMu.RUnlock()
	go func() {
		r.sampleObserverMu.RLock()
		defer r.sampleObserverMu.RUnlock()

		if generation == r.sampleObserverGeneration {
			observer(observation)
		}
	}()
}

func (r *batteryPowerRegulator) batteryFeedbackErrorLocked(now time.Time, sample batteryPowerSample, cause error) error {
	lastValidAge := "unavailable"
	if !r.lastBatterySample.FinishedAt.IsZero() {
		lastValidAge = now.Sub(r.lastBatterySample.FinishedAt).String()
	}

	return fmt.Errorf("%w; battery read duration: %s; last valid sample age: %s",
		cause, sample.FinishedAt.Sub(sample.StartedAt), lastValidAge)
}

func (r *batteryPowerRegulator) handleUnavailableBatteryFeedbackLocked(now time.Time, grid batteryPowerSample, cause error) bool {
	if r.appliedCommand == 0 ||
		r.phase != batteryPowerCharging && r.phase != batteryPowerDischarging ||
		r.lastBatterySample.FinishedAt.IsZero() ||
		now.Sub(r.lastBatterySample.FinishedAt) >= batteryPowerFeedbackGrace {
		return false
	}

	remaining := batteryPowerFeedbackGrace - now.Sub(r.lastBatterySample.FinishedAt)
	r.log.ERROR.Printf(
		"battery power control: battery feedback unavailable: %v; holding %.0fW for up to %s",
		cause, r.appliedCommand, remaining.Round(time.Second),
	)

	if !r.policy.forceCharge {
		target := r.gridTargetLocked(r.phase)
		rawError := grid.Value - target
		if command, ok := immediateBatteryPowerRetreat(r.appliedCommand, rawError); ok {
			if math.Abs(command) < batteryPowerWriteThreshold {
				command = 0
			}
			if command != r.appliedCommand &&
				(command == 0 || math.Abs(command-r.appliedCommand) >= batteryPowerWriteThreshold) {
				if err := r.applyCommandLocked(command, false, "degraded feedback safety retreat"); err != nil {
					r.markFaultLocked("safety retreat failed", err)
				}
			}
		}
	}

	return true
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
		return r.policy.chargeAllowed && r.policy.chargeLimit > 0 &&
			!r.clock.Now().Before(r.chargeBlockedUntil)
	case batteryPowerDischarging:
		return r.policy.dischargeAllowed && r.policy.dischargeLimit > 0 &&
			!r.clock.Now().Before(r.dischargeBlockedUntil)
	default:
		return true
	}
}

func (r *batteryPowerRegulator) directionBlockedUntilLocked(direction batteryPowerPhase) *time.Time {
	switch direction {
	case batteryPowerCharging:
		return &r.chargeBlockedUntil
	case batteryPowerDischarging:
		return &r.dischargeBlockedUntil
	default:
		return nil
	}
}

func (r *batteryPowerRegulator) gridTargetLocked(direction batteryPowerPhase) float64 {
	switch direction {
	case batteryPowerCharging:
		return -r.policy.residualPower / 4
	case batteryPowerDischarging:
		return batteryPowerDischargeGridTarget
	default:
		return 0
	}
}

func (r *batteryPowerRegulator) controlDemandLocked(gridPower float64) (batteryPowerPhase, float64, bool) {
	switch r.phase {
	case batteryPowerCharging:
		return batteryPowerCharging, r.gridTargetLocked(batteryPowerCharging), true
	case batteryPowerDischarging:
		return batteryPowerDischarging, r.gridTargetLocked(batteryPowerDischarging), true
	case batteryPowerNeutral:
		chargeTarget := r.gridTargetLocked(batteryPowerCharging)
		chargeStartTarget := min(chargeTarget, 0)
		dischargeTarget := r.gridTargetLocked(batteryPowerDischarging)
		dischargeStartTarget := max(dischargeTarget, 0)

		switch {
		case r.directionAllowedLocked(batteryPowerCharging) && gridPower < chargeStartTarget-batteryPowerStartDeadband:
			return batteryPowerCharging, chargeTarget, true
		case r.directionAllowedLocked(batteryPowerDischarging) && gridPower > dischargeStartTarget+batteryPowerStartDeadband:
			return batteryPowerDischarging, dischargeTarget, true
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

func batteryPowerIncreaseDemand(direction batteryPowerPhase, rawError float64) bool {
	switch direction {
	case batteryPowerCharging:
		return rawError < -batteryPowerActiveDeadband
	case batteryPowerDischarging:
		return rawError > batteryPowerActiveDeadband
	default:
		return false
	}
}

func batteryPowerIncreaseParameters(direction batteryPowerPhase, gridPower float64, fastImportConfirmed bool) (float64, float64) {
	if direction == batteryPowerDischarging && gridPower > batteryPowerFastImportThreshold {
		if !fastImportConfirmed {
			return batteryPowerFastDischargeGain, batteryPowerFastDischargeFirstStep
		}
		return batteryPowerFastDischargeGain, batteryPowerFastDischargeMaxStep
	}
	return batteryPowerGain, batteryPowerMaxIncreaseStep
}

func (r *batteryPowerRegulator) increasedCommandLocked(direction batteryPowerPhase, gridPower, rawError float64, fastImportConfirmed bool) (float64, bool) {
	if !batteryPowerIncreaseDemand(direction, rawError) {
		return 0, false
	}

	// Actuator anti-windup: refuse a further charging increase unless
	// measured feedback has genuinely caught up to the currently applied
	// command. Feedback that is still trailing, or that has turned
	// materially wrong-direction, must not permit escalation: an increased
	// command must never be written on a bad reading, only on confirmed
	// catch-up. This is unconditional and stateless, so it applies before
	// every charging increase, not only after a saturation hold; a fresh
	// start from neutral (applied command zero) is naturally exempt.
	if direction == batteryPowerCharging && r.appliedCommand < 0 {
		if !batteryChargeFeedbackCaughtUp(r.appliedCommand, r.lastBatterySample.Value) {
			return 0, false
		}
	}

	gain, maxStep := batteryPowerIncreaseParameters(direction, gridPower, fastImportConfirmed)
	var delta float64
	switch direction {
	case batteryPowerCharging:
		delta = max(rawError*gain, -maxStep)
	case batteryPowerDischarging:
		delta = min(rawError*gain, maxStep)
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

func (r *batteryPowerRegulator) sustainedFastImportLocked(sampledAt time.Time, gridPower float64) bool {
	if gridPower <= batteryPowerFastImportThreshold {
		r.lastFastImportAt = time.Time{}
		return false
	}

	confirmed := !r.lastFastImportAt.IsZero() &&
		sampledAt.After(r.lastFastImportAt) &&
		sampledAt.Sub(r.lastFastImportAt) <= batteryPowerFastImportMaxGap
	r.lastFastImportAt = sampledAt
	return confirmed
}

func (r *batteryPowerRegulator) overridePendingDischargeReductionLocked(gridPower float64, fastImportConfirmed bool) bool {
	pending := r.pendingCommand
	if !fastImportConfirmed || pending == nil || pending.Command <= 0 || pending.PreviousCommand <= pending.Command {
		return false
	}

	rawError := gridPower - r.gridTargetLocked(batteryPowerDischarging)
	command, ok := r.increasedCommandLocked(batteryPowerDischarging, gridPower, rawError, true)
	if !ok {
		return false
	}

	if err := r.applyCommandLocked(command, false, "sustained import overrides pending reduction"); err != nil {
		r.markFaultLocked("import override failed", err)
	}
	return true
}

func (r *batteryPowerRegulator) rollbackUndemandedIncreaseLocked(gridPower float64) bool {
	pending := r.pendingCommand
	if pending == nil || !magnitudeIncreased(pending) {
		return false
	}

	direction := directionForCommand(pending.Command)
	rawError := gridPower - r.gridTargetLocked(direction)
	if batteryPowerIncreaseDemand(direction, rawError) {
		return false
	}

	if err := r.applyCommandGatedLocked(pending.PreviousCommand, false, false, "pending increase no longer demanded"); err != nil {
		r.markFaultLocked("command rollback failed", err)
	}
	return true
}

func (r *batteryPowerRegulator) observeNeutralLocked(sample batteryPowerSample) bool {
	if !r.neutralRequired {
		return true
	}
	if !r.neutralSampleObservedLocked(sample) {
		return false
	}

	r.neutralRequired = false
	return true
}

func (r *batteryPowerRegulator) neutralSampleObservedLocked(sample batteryPowerSample) bool {
	return sample.StartedAt.After(r.neutralSince) &&
		math.Abs(sample.Value) <= batteryPowerNeutralTolerance
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
			r.acknowledgePendingCommandLocked()
		case pending.Command > 0 && sample.Value <= pending.Command+tolerance:
			r.acknowledgePendingCommandLocked()
		case batteryPowerReductionResponded(pending, sample.Value):
			r.acknowledgePendingCommandLocked()
		}
		return
	}

	switch {
	case delta < 0 && sample.Value <= pending.Command+tolerance:
		r.acknowledgePendingCommandLocked()
		return
	case delta > 0 && sample.Value >= pending.Command-tolerance:
		r.acknowledgePendingCommandLocked()
		return
	}

	movement := sample.Value - pending.BaselinePower
	required := max(batteryPowerAckMovementMinimum, math.Abs(delta)*batteryPowerAckMovementPercentage)
	if delta < 0 && movement <= -required || delta > 0 && movement >= required {
		r.acknowledgePendingCommandLocked()
	}
}

func (r *batteryPowerRegulator) acknowledgePendingCommandLocked() {
	pending := r.pendingCommand
	direction := directionForCommand(r.pendingCommand.Command)
	blockedUntil := r.directionBlockedUntilLocked(direction)
	if magnitudeIncreased(pending) && blockedUntil != nil && !blockedUntil.IsZero() {
		*blockedUntil = time.Time{}
		r.log.DEBUG.Printf("battery power control: %s command acknowledged; cooldown history cleared", direction)
	}
	r.pendingCommand = nil
}

// batteryPowerCommandMaterial reports whether a command differs enough from the
// latest measured battery baseline to require acknowledgement proof. Huawei
// battery telemetry carries enough noise that small actuator changes cannot be
// reliably distinguished from measurement noise.
func batteryPowerCommandMaterial(command, baselinePower float64) bool {
	return math.Abs(command-baselinePower) >= max(batteryPowerMaterialFloor, batteryPowerMaterialPercentage*math.Abs(command))
}

func magnitudeIncreased(pending *pendingBatteryPowerCommand) bool {
	return math.Abs(pending.Command) > math.Abs(pending.PreviousCommand)
}

// batteryPowerReductionResponded accepts an immaterial residual gap only after
// feedback has crossed to the safer side of the previous command.
func batteryPowerReductionResponded(pending *pendingBatteryPowerCommand, batteryPower float64) bool {
	if pending.Command == 0 || math.Abs(pending.Command) >= math.Abs(pending.PreviousCommand) {
		return false
	}

	switch {
	case pending.Command < 0 && batteryPower <= pending.PreviousCommand:
		return false
	case pending.Command > 0 && batteryPower >= pending.PreviousCommand:
		return false
	}

	return !batteryPowerCommandMaterial(pending.Command, batteryPower)
}

// batteryChargeFeedbackTrails reports whether measured battery power
// materially trails a charging command: charging is happening but has not
// reached the commanded magnitude, consistent with Huawei BMS taper or
// collapse under persistent export rather than a wrong-direction response.
// A small positive reading up to the existing neutral tolerance is treated
// as taper noise; a larger positive reading is material discharging and is
// not trailing.
func batteryChargeFeedbackTrails(command, batteryPower float64) bool {
	if command >= 0 || batteryPower <= command || batteryPower > batteryPowerNeutralTolerance {
		return false
	}
	return batteryPowerCommandMaterial(command, batteryPower)
}

// batteryChargeFeedbackCaughtUp reports whether measured battery power has
// genuinely caught up to a held charging command, the only condition that
// may release the actuator anti-windup gate. It deliberately does not
// return true for wrong-direction feedback: a materially positive reading
// (beyond the neutral tolerance) means the battery is not delivering the
// commanded charge at all, so the gate must keep blocking rather than let a
// bad reading be mistaken for catch-up and permit escalation.
func batteryChargeFeedbackCaughtUp(command, batteryPower float64) bool {
	if batteryPower <= command {
		return true
	}
	if batteryPower > batteryPowerNeutralTolerance {
		return false
	}
	return !batteryPowerCommandMaterial(command, batteryPower)
}

// chargingEstablishedLocked reports whether charging was already materially
// underway before a pending increase, based on the actuator's own previous
// command and the measured baseline at the time the increase was applied.
// Neither an unproven immaterial previous command nor a baseline near
// neutral counts as established.
func chargingEstablishedLocked(pending *pendingBatteryPowerCommand) bool {
	return pending.PreviousCommand < 0 && pending.BaselinePower < 0 &&
		batteryPowerCommandMaterial(0, pending.BaselinePower)
}

func (r *batteryPowerRegulator) socDiagnosticLocked() string {
	if r.policy.socLimitsValid {
		return fmt.Sprintf("%.1f%% (limits %.1f%%..%.1f%%)", r.policy.soc, r.policy.minSoc, r.policy.maxSoc)
	}
	return "unavailable"
}

// chargingSaturationHoldLocked reports whether a timed-out charging
// magnitude increase should be held as a safe actuator saturation rather
// than faulted. It applies only when charging was already established
// before the increase and the current battery feedback materially trails
// the applied command without indicating a wrong-direction response. This
// decision is purely feedback-based; it never applies to discharge and does
// not depend on any SoC threshold. The applied command and phase are left
// untouched: no state is recorded here. The stateless anti-windup gate in
// increasedCommandLocked re-evaluates applied-command-vs-feedback on every
// later cycle and naturally keeps blocking a further increase until
// feedback catches up.
func (r *batteryPowerRegulator) chargingSaturationHoldLocked(now time.Time, grid, battery batteryPowerSample) bool {
	pending := r.pendingCommand
	if pending.Command >= 0 || !magnitudeIncreased(pending) || !chargingEstablishedLocked(pending) {
		return false
	}
	if !batteryChargeFeedbackTrails(pending.Command, battery.Value) {
		return false
	}

	r.pendingCommand = nil
	r.log.DEBUG.Printf(
		"battery power control: charging saturation hold: command=%.0fW previous=%.0fW battery-baseline=%.0fW battery=%.0fW grid=%.0fW soc=%s elapsed=%s",
		pending.Command, pending.PreviousCommand, pending.BaselinePower, battery.Value, grid.Value,
		r.socDiagnosticLocked(), now.Sub(pending.AppliedAt).Round(time.Second),
	)
	return true
}

func (r *batteryPowerRegulator) pendingTimedOutLocked(now time.Time) bool {
	return r.pendingCommand != nil &&
		now.Sub(r.pendingCommand.AppliedAt) >= batteryPowerMaxSettleTime
}

// armDirectionFailureCooldownLocked arms (or extends) the first/repeated
// cooldown for a failed charging or discharging magnitude response and
// reports the cooldown duration actually armed plus whether this is a
// repeated failure, for consistent logging across failure paths. When arm
// is false, no cooldown is set (a failed reduction never blocks a
// direction), but repeated is still reported for diagnostics.
func (r *batteryPowerRegulator) armDirectionFailureCooldownLocked(direction batteryPowerPhase, now time.Time, arm bool) (time.Duration, bool) {
	blockedUntil := r.directionBlockedUntilLocked(direction)
	repeated := blockedUntil != nil && !blockedUntil.IsZero()
	var cooldown time.Duration
	if arm && blockedUntil != nil {
		cooldown = batteryPowerFirstCooldown
		if repeated {
			cooldown = batteryPowerRepeatedCooldown
		}
		*blockedUntil = now.Add(cooldown)
	}
	return cooldown, repeated
}

func (r *batteryPowerRegulator) stopForCommandTimeoutLocked(now time.Time, grid, battery batteryPowerSample) {
	pending := r.pendingCommand
	direction := directionForCommand(pending.Command)
	cooldown, repeated := r.armDirectionFailureCooldownLocked(direction, now, magnitudeIncreased(pending))

	soc := r.socDiagnosticLocked()
	cooldownDetails := "none"
	if cooldown > 0 {
		cooldownDetails = cooldown.String()
	}
	details := fmt.Sprintf(
		"direction=%s command=%.0fW previous=%.0fW battery-baseline=%.0fW battery-final=%.0fW grid=%.0fW soc=%s elapsed=%s cooldown=%s next=neutral-feedback",
		direction, pending.Command, pending.PreviousCommand, pending.BaselinePower, battery.Value, grid.Value, soc,
		now.Sub(pending.AppliedAt).Round(time.Second), cooldownDetails,
	)

	stopErr := r.applyCommandLocked(0, true, "command acknowledgement timed out")
	r.phase = batteryPowerFaultStopping
	if cooldown > 0 {
		r.log.DEBUG.Printf(
			"battery power control: %s blocked for %s after command acknowledgement timeout",
			direction, cooldown,
		)
	}
	switch {
	case stopErr != nil:
		r.log.ERROR.Printf("battery power control: command acknowledgement timed out: %s: %v", details, stopErr)
	case repeated && cooldown > 0:
		r.log.DEBUG.Printf("battery power control: repeated command acknowledgement timeout: %s", details)
	default:
		r.log.ERROR.Printf("battery power control: command acknowledgement timed out: %s", details)
	}
}

// stopForChargingWrongDirectionLocked hard-stops established charging that
// has turned materially wrong-direction (battery discharging beyond the
// neutral tolerance) while no pending command is in flight to catch it via
// the acknowledgement timeout, e.g. after a prior saturation hold or a
// normal acknowledged command. It is treated the same as a failed charging
// magnitude increase, with the same first/repeated cooldown, so rearm
// cannot immediately repeat it.
func (r *batteryPowerRegulator) stopForChargingWrongDirectionLocked(now time.Time, grid, battery batteryPowerSample) {
	direction := batteryPowerCharging
	cooldown, repeated := r.armDirectionFailureCooldownLocked(direction, now, true)

	soc := r.socDiagnosticLocked()
	details := fmt.Sprintf(
		"direction=%s command=%.0fW battery=%.0fW grid=%.0fW soc=%s cooldown=%s next=neutral-feedback",
		direction, r.appliedCommand, battery.Value, grid.Value, soc, cooldown,
	)

	stopErr := r.applyCommandLocked(0, true, "charging feedback materially wrong-direction")
	r.phase = batteryPowerFaultStopping
	switch {
	case stopErr != nil:
		r.log.ERROR.Printf("battery power control: charging feedback wrong-direction: %s: %v", details, stopErr)
	case repeated:
		r.log.DEBUG.Printf("battery power control: repeated charging wrong-direction stop: %s", details)
	default:
		r.log.ERROR.Printf("battery power control: charging feedback wrong-direction: %s", details)
	}
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

	// Same stateless actuator anti-windup gate as normal charging control:
	// refuse a further magnitude increase unless feedback has genuinely
	// caught up with the currently applied command. A fresh start from
	// neutral (applied command zero) remains exempt. Wrong-direction
	// feedback is already handled earlier in tick() before force charge is
	// ever advanced, so only trailing feedback can reach here.
	if r.appliedCommand < 0 && !batteryChargeFeedbackCaughtUp(r.appliedCommand, battery.Value) {
		r.maybeRefreshCommandLocked(now)
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
	return r.applyCommandGatedLocked(command, force, true, reason)
}

// applyCommandGatedLocked applies command like applyCommandLocked but lets the
// caller bypass the materiality gate. The rollback of an undemanded increase
// must always arm a pending reduction so a following increase still waits for
// proof that the rollback itself took effect, regardless of how close the
// rollback target already sits to the latest battery reading.
func (r *batteryPowerRegulator) applyCommandGatedLocked(command float64, force, requireMaterial bool, reason string) error {
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
		var stopErr error
		if command != 0 {
			stopErr = r.battery.controller.SetBatteryPower(0)
			if stopErr == nil {
				r.appliedCommand = 0
				r.initialized = true
				r.lastWriteAt = r.clock.Now()
				r.lastCommandReason = reason + " failed; best-effort zero"
			}
			r.recordStopAttemptLocked(r.clock.Now(), stopErr)
		} else {
			r.recordStopAttemptLocked(r.clock.Now(), err)
		}
		r.pendingCommand = nil
		r.phase = batteryPowerFaultStopping
		return errors.Join(fmt.Errorf("%s: %w", r.battery.name, err), stopErr)
	}

	now := r.clock.Now()
	r.appliedCommand = command
	r.initialized = true
	r.lastWriteAt = now
	r.lastCommandReason = reason
	if command == 0 {
		r.recordStopAttemptLocked(now, nil)
	}

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

	if command != 0 && command != previous && (!requireMaterial || batteryPowerCommandMaterial(command, baseline)) {
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
	if r.phase == batteryPowerFaultStopping && !r.stopRetryDueLocked(r.clock.Now()) {
		return nil
	}
	return r.applyCommandLocked(0, true, reason)
}

func (r *batteryPowerRegulator) stopAndFaultLocked(reason string, cause error) {
	if r.phase == batteryPowerFaultStopping && r.knownStoppedLocked() {
		return
	}
	if r.phase == batteryPowerFaultStopping && !r.stopRetryDueLocked(r.clock.Now()) {
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
	if !r.knownStoppedLocked() {
		if !r.stopRetryDueLocked(r.clock.Now()) {
			return
		}
		if err := r.applyCommandLocked(0, true, "fault stop retry"); err != nil {
			r.log.ERROR.Printf("battery power control: stop retry: %v", err)
		} else if r.sampleObserverRecover {
			r.sampleObserverEnabled = true
			r.sampleObserverRecover = false
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
	r.log.DEBUG.Printf("battery power control: rearmed at battery %.0fW, grid %.0fW", battery.Value, grid.Value)
}

// knownStoppedLocked reports whether a successful zero is known to be in
// effect. A failed stop leaves appliedCommand at 0, so that value alone
// cannot prove the actuator is idle.
func (r *batteryPowerRegulator) knownStoppedLocked() bool {
	return r.initialized && r.appliedCommand == 0 && r.stopFailureSince.IsZero()
}

func (r *batteryPowerRegulator) recordStopAttemptLocked(now time.Time, err error) {
	if err == nil {
		r.stopFailureSince = time.Time{}
		r.lastStopAttemptAt = time.Time{}
		return
	}
	if r.stopFailureSince.IsZero() {
		r.stopFailureSince = now
	}
	r.lastStopAttemptAt = now
}

func (r *batteryPowerRegulator) stopRetryDueLocked(now time.Time) bool {
	if r.stopFailureSince.IsZero() {
		return true
	}
	if now.Sub(r.lastStopAttemptAt) < batteryPowerControlInterval {
		return false
	}
	return now.Sub(r.stopFailureSince) < batteryPowerStopRetrySafetyWindow ||
		now.Sub(r.lastStopAttemptAt) >= batteryPowerStopRetryInterval
}

func (r *batteryPowerRegulator) resetControlLocked() {
	r.pendingCommand = nil
	r.lastFastImportAt = time.Time{}
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
