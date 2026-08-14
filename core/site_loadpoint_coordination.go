package core

import (
	"errors"
	"math"
	"slices"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/loadpoint"
)

type pvPriorityState uint8

const (
	pvPriorityInactive pvPriorityState = iota
	pvPriorityQualifying
	pvPriorityHandover
)

type loadpointDemandClaim struct {
	target    float64
	observed  float64
	notified  float64
	expiresAt time.Time
}

type loadpointDemandCoordinator interface {
	prepareLoadpointDemand(loadpoint.API, float64, float64, time.Time, time.Time) (bool, error)
	commitLoadpointDemand(loadpoint.API)
	observeLoadpointDemand(loadpoint.API, float64, bool, time.Time) error
	cancelLoadpointDemand(loadpoint.API, time.Time) error
	pendingLoadpointDemand(loadpoint.API, time.Time) float64
}

func (c loadpointDemandClaim) remaining(now time.Time) float64 {
	if !c.expiresAt.After(now) {
		return 0
	}
	return max(0, c.target-c.observed)
}

func (site *Site) initLoadpointCoordination(interval time.Duration) {
	site.coordinationMu.Lock()
	defer site.coordinationMu.Unlock()

	site.loadpointUpdateCycle = max(chargerSwitchDuration, interval*time.Duration(max(1, len(site.loadpoints))))
	if site.loadpointDemandClaims == nil {
		site.loadpointDemandClaims = make(map[loadpoint.API]loadpointDemandClaim)
	}
	if site.loadpointDemandBackup == nil {
		site.loadpointDemandBackup = make(map[loadpoint.API]*loadpointDemandClaim)
	}
	if site.pvPriorityStates == nil {
		site.pvPriorityStates = make(map[*Loadpoint]pvPriorityState)
	}
	if site.reevaluationSet == nil {
		site.reevaluationSet = make(map[*Loadpoint]struct{})
	}
	if site.reevaluationWake == nil {
		site.reevaluationWake = make(chan struct{}, 1)
	}
	if site.coordinationChanged == nil {
		site.coordinationChanged = make(chan struct{}, 1)
	}
}

func (site *Site) orderedStartupLoadpoints() []*Loadpoint {
	res := slices.Clone(site.loadpoints)
	slices.SortStableFunc(res, func(a, b *Loadpoint) int {
		return b.EffectivePriority() - a.EffectivePriority()
	})
	return res
}

func (site *Site) beginStartupCoordination() {
	site.coordinationMu.Lock()
	site.startupCoordination = true
	site.coordinationMu.Unlock()
}

func (site *Site) endStartupCoordination() {
	site.coordinationMu.Lock()
	site.startupCoordination = false
	site.reevaluationQueue = nil
	clear(site.reevaluationSet)
	site.coordinationMu.Unlock()
	site.signalCoordinationChange()
}

func (site *Site) beginLoadpointUpdate(lp updater) {
	concrete, _ := lp.(*Loadpoint)

	site.coordinationMu.Lock()
	site.activeLoadpointUpdate = concrete
	if concrete != nil {
		delete(site.reevaluationSet, concrete)
	}
	site.coordinationMu.Unlock()
}

func (site *Site) endLoadpointUpdate(lp updater) {
	concrete, _ := lp.(*Loadpoint)

	site.coordinationMu.Lock()
	if site.activeLoadpointUpdate == concrete {
		site.activeLoadpointUpdate = nil
	}
	site.coordinationMu.Unlock()

	if concrete != nil {
		site.updatePVPriorityState(concrete)
	}
}

func (site *Site) updatePVPriorityState(lp *Loadpoint) {
	state := lp.pvPriorityState(site.loadpointHandoverWindow())

	site.coordinationMu.Lock()
	if site.pvPriorityStates == nil {
		site.pvPriorityStates = make(map[*Loadpoint]pvPriorityState)
	}
	previous, known := site.pvPriorityStates[lp]
	site.pvPriorityStates[lp] = state
	startup := site.startupCoordination
	site.coordinationMu.Unlock()

	if !startup && known && previous != state {
		site.schedulePriorityDependents(lp)
	}
	if !startup && (!known || previous != state) {
		site.signalCoordinationChange()
	}
}

func (site *Site) loadpointHandoverWindow() time.Duration {
	site.coordinationMu.Lock()
	defer site.coordinationMu.Unlock()

	if site.loadpointUpdateCycle > 0 {
		return site.loadpointUpdateCycle
	}
	return chargerSwitchDuration
}

func (site *Site) schedulePriorityDependents(source loadpoint.API) {
	priority := source.EffectivePriority()
	var candidates []*Loadpoint
	for _, lp := range site.loadpoints {
		if lp != source && lp.GetMode() == api.ModePV && lp.EffectivePriority() < priority {
			candidates = append(candidates, lp)
		}
	}

	site.coordinationMu.Lock()
	if site.startupCoordination {
		site.coordinationMu.Unlock()
		return
	}
	if site.reevaluationSet == nil {
		site.reevaluationSet = make(map[*Loadpoint]struct{})
	}
	if site.reevaluationWake == nil {
		site.reevaluationWake = make(chan struct{}, 1)
	}

	for _, lp := range candidates {
		if lp == site.activeLoadpointUpdate {
			continue
		}
		if _, exists := site.reevaluationSet[lp]; exists {
			continue
		}
		site.reevaluationSet[lp] = struct{}{}
		site.reevaluationQueue = append(site.reevaluationQueue, lp)
	}
	wake := site.reevaluationWake
	hasWork := len(site.reevaluationQueue) > 0
	site.coordinationMu.Unlock()

	if hasWork && wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func (site *Site) nextLoadpointReevaluation() *Loadpoint {
	site.coordinationMu.Lock()
	defer site.coordinationMu.Unlock()

	for len(site.reevaluationQueue) > 0 {
		lp := site.reevaluationQueue[0]
		site.reevaluationQueue = site.reevaluationQueue[1:]
		if _, pending := site.reevaluationSet[lp]; !pending {
			continue
		}
		delete(site.reevaluationSet, lp)
		return lp
	}
	return nil
}

func (site *Site) updateScheduledLoadpoints() {
	for {
		lp := site.nextLoadpointReevaluation()
		if lp == nil {
			return
		}
		site.update(lp)
	}
}

func (site *Site) prepareLoadpointDemand(
	lp loadpoint.API,
	targetPower, observedPower float64,
	now time.Time,
	expiresAt time.Time,
) (bool, error) {
	if math.IsNaN(targetPower) || math.IsInf(targetPower, 0) ||
		math.IsNaN(observedPower) || math.IsInf(observedPower, 0) ||
		targetPower < 0 || observedPower < 0 || expiresAt.IsZero() {
		return false, errors.New("invalid loadpoint demand")
	}

	claim := loadpointDemandClaim{
		target:    targetPower,
		observed:  observedPower,
		expiresAt: expiresAt,
	}
	claim.notified = claim.remaining(now)
	if claim.remaining(now) <= standbyPower || !expiresAt.After(now) {
		return false, nil
	}

	site.coordinationMu.Lock()
	if site.loadpointDemandClaims == nil {
		site.loadpointDemandClaims = make(map[loadpoint.API]loadpointDemandClaim)
	}
	if site.loadpointDemandBackup == nil {
		site.loadpointDemandBackup = make(map[loadpoint.API]*loadpointDemandClaim)
	}

	previous, existed := site.loadpointDemandClaims[lp]
	if existed {
		backup := previous
		site.loadpointDemandBackup[lp] = &backup
	} else {
		site.loadpointDemandBackup[lp] = nil
	}
	site.loadpointDemandClaims[lp] = claim
	total, until := site.loadpointDemandLocked(now)
	if regulator := site.batteryPowerRegulator; regulator != nil {
		if err := regulator.setLoadpointDemand(total, until); err != nil {
			if existed {
				site.loadpointDemandClaims[lp] = previous
			} else {
				delete(site.loadpointDemandClaims, lp)
			}
			delete(site.loadpointDemandBackup, lp)
			site.coordinationMu.Unlock()
			return false, err
		}
	}
	site.coordinationMu.Unlock()

	site.signalCoordinationChange()
	return true, nil
}

func (site *Site) commitLoadpointDemand(lp loadpoint.API) {
	site.coordinationMu.Lock()
	delete(site.loadpointDemandBackup, lp)
	site.coordinationMu.Unlock()
}

func (site *Site) observeLoadpointDemand(
	lp loadpoint.API,
	observedPower float64,
	valid bool,
	now time.Time,
) error {
	site.coordinationMu.Lock()
	claim, exists := site.loadpointDemandClaims[lp]
	if !exists {
		site.coordinationMu.Unlock()
		return nil
	}

	previous := claim.remaining(now)
	expired := !claim.expiresAt.After(now) && claim.target-claim.observed > standbyPower
	materialReduction := false
	switch {
	case !claim.expiresAt.After(now):
		delete(site.loadpointDemandClaims, lp)
	case valid:
		claim.observed = max(claim.observed, observedPower)
		if claim.remaining(now) <= standbyPower {
			materialReduction = true
			delete(site.loadpointDemandClaims, lp)
		} else {
			materialReduction = materialLoadpointDemandReduction(lp, claim.notified, claim.remaining(now))
			if materialReduction {
				claim.notified = claim.remaining(now)
			}
			site.loadpointDemandClaims[lp] = claim
		}
	}

	total, until := site.loadpointDemandLocked(now)
	if regulator := site.batteryPowerRegulator; regulator != nil {
		if err := regulator.setLoadpointDemand(total, until); err != nil {
			site.coordinationMu.Unlock()
			return err
		}
	}
	current := claim.remaining(now)
	if _, exists := site.loadpointDemandClaims[lp]; !exists {
		current = 0
	}
	site.coordinationMu.Unlock()

	if expired || materialReduction {
		site.schedulePriorityDependents(lp)
	}
	if expired || current != previous {
		site.signalCoordinationChange()
	}
	return nil
}

func materialLoadpointDemandReduction(lp loadpoint.API, previous, current float64) bool {
	if previous <= current {
		return false
	}
	if current <= standbyPower {
		return true
	}

	threshold := batteryPowerWriteThreshold
	if concrete, ok := lp.(*Loadpoint); ok {
		threshold = max(threshold, concrete.EffectiveStepPower())
	}
	return previous-current >= threshold
}

func (site *Site) cancelLoadpointDemand(lp loadpoint.API, now time.Time) error {
	site.coordinationMu.Lock()
	current, existed := site.loadpointDemandClaims[lp]
	backup, prepared := site.loadpointDemandBackup[lp]
	if !prepared {
		delete(site.loadpointDemandClaims, lp)
	} else if backup == nil {
		delete(site.loadpointDemandClaims, lp)
	} else {
		site.loadpointDemandClaims[lp] = *backup
	}
	delete(site.loadpointDemandBackup, lp)
	updated, remains := site.loadpointDemandClaims[lp]
	changed := existed != remains || existed && current != updated
	if !changed {
		site.coordinationMu.Unlock()
		return nil
	}

	total, until := site.loadpointDemandLocked(now)
	if regulator := site.batteryPowerRegulator; regulator != nil {
		if err := regulator.setLoadpointDemand(total, until); err != nil {
			site.coordinationMu.Unlock()
			return err
		}
	}
	site.coordinationMu.Unlock()

	site.schedulePriorityDependents(lp)
	site.signalCoordinationChange()
	return nil
}

func (site *Site) loadpointDemandLocked(now time.Time) (float64, time.Time) {
	var total float64
	var until time.Time
	for _, claim := range site.loadpointDemandClaims {
		remaining := claim.remaining(now)
		if remaining <= standbyPower {
			continue
		}
		total += remaining
		if claim.expiresAt.After(until) {
			until = claim.expiresAt
		}
	}
	return total, until
}

func (site *Site) pendingLoadpointDemand(lp loadpoint.API, now time.Time) float64 {
	site.coordinationMu.Lock()
	defer site.coordinationMu.Unlock()

	claim, exists := site.loadpointDemandClaims[lp]
	if !exists {
		return 0
	}
	return claim.remaining(now)
}

func (site *Site) signalCoordinationChange() {
	site.coordinationMu.Lock()
	if site.coordinationChanged == nil {
		site.coordinationChanged = make(chan struct{}, 1)
	}
	changed := site.coordinationChanged
	site.coordinationMu.Unlock()

	select {
	case changed <- struct{}{}:
	default:
	}
}

func (site *Site) nextCoordinationDeadline() time.Time {
	site.coordinationMu.Lock()
	var next time.Time
	for _, claim := range site.loadpointDemandClaims {
		if claim.target-claim.observed > standbyPower &&
			(next.IsZero() || claim.expiresAt.Before(next)) {
			next = claim.expiresAt
		}
	}

	updateCycle := site.loadpointUpdateCycle
	var qualifying []*Loadpoint
	for lp, state := range site.pvPriorityStates {
		if state == pvPriorityQualifying {
			qualifying = append(qualifying, lp)
		}
	}
	site.coordinationMu.Unlock()

	for _, lp := range qualifying {
		if deadline, ok := lp.pvHandoverAt(updateCycle); ok &&
			(next.IsZero() || deadline.Before(next)) {
			next = deadline
		}
	}
	return next
}

func (site *Site) processCoordinationDeadlines(now time.Time) error {
	site.coordinationMu.Lock()
	var expired []loadpoint.API
	for lp, claim := range site.loadpointDemandClaims {
		if claim.target-claim.observed > standbyPower && !claim.expiresAt.After(now) {
			expired = append(expired, lp)
			delete(site.loadpointDemandClaims, lp)
			delete(site.loadpointDemandBackup, lp)
		}
	}
	total, until := site.loadpointDemandLocked(now)
	var regulatorErr error
	if regulator := site.batteryPowerRegulator; regulator != nil {
		regulatorErr = regulator.setLoadpointDemand(total, until)
	}

	var qualifying []*Loadpoint
	for lp, state := range site.pvPriorityStates {
		if state == pvPriorityQualifying {
			qualifying = append(qualifying, lp)
		}
	}
	site.coordinationMu.Unlock()

	for _, lp := range expired {
		site.schedulePriorityDependents(lp)
	}
	for _, lp := range qualifying {
		site.updatePVPriorityState(lp)
	}
	return regulatorErr
}
