package core

import (
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/util"
)

const testEnableDelay = time.Minute

func newPVLoadpoint(prio int, mode api.ChargeMode, status api.ChargeStatus, enabled bool, timer time.Time) *Loadpoint {
	lp := &Loadpoint{
		log:        util.NewLogger("lp"),
		clock:      clock.NewMock(),
		minCurrent: minA,
		maxCurrent: maxA,
		phases:     1,
		mode:       mode,
		status:     status,
		enabled:    enabled,
		pvTimer:    timer,
		priority:   prio,
	}
	lp.Enable.Delay = testEnableDelay
	return lp
}

// pvTimerStarted returns a timer start that has been running for the given duration
func pvTimerStarted(running time.Duration) time.Time {
	return clock.NewMock().Now().Add(-running)
}

func TestPvChargeStarting(t *testing.T) {
	now := clock.NewMock().Now()
	settled := pvTimerStarted(testEnableDelay / 2)

	// enable timer running but car already full (soc at default 100% limit): not starting up
	enablePendingFull := newPVLoadpoint(0, api.ModePV, api.StatusB, false, settled)
	enablePendingFull.vehicleSoc = 100

	tc := []struct {
		name     string
		lp       *Loadpoint
		starting bool
	}{
		{"enable timer running", newPVLoadpoint(0, api.ModePV, api.StatusB, false, settled), true},
		{"enable timer just restarted", newPVLoadpoint(0, api.ModePV, api.StatusB, false, now), false},
		{"enabled not charging", newPVLoadpoint(0, api.ModePV, api.StatusB, true, time.Time{}), false},
		{"enabled and charging", newPVLoadpoint(0, api.ModePV, api.StatusC, true, time.Time{}), false},
		{"disabled idle", newPVLoadpoint(0, api.ModePV, api.StatusB, false, time.Time{}), false},
		{"disconnected", newPVLoadpoint(0, api.ModePV, api.StatusA, false, settled), false},
		{"not pv mode", newPVLoadpoint(0, api.ModeNow, api.StatusB, false, settled), false},
		{"enable pending but car full", enablePendingFull, false},
	}

	for _, tc := range tc {
		if got := tc.lp.PvChargeStarting(); got != tc.starting {
			t.Errorf("%s: want %v, got %v", tc.name, tc.starting, got)
		}
	}
}

func TestReservedPVPower(t *testing.T) {
	Voltage = 230

	// Timer must already have survived half the enable delay (#32778).
	high := newPVLoadpoint(1, api.ModePV, api.StatusB, false, pvTimerStarted(10*time.Minute))
	high.Enable.Delay = 20 * time.Minute
	high.chargePower = 167
	high.demandPower = 167
	high.demandValid = true
	low := newPVLoadpoint(0, api.ModePV, api.StatusB, false, time.Time{})
	low.clock = high.clock

	site := &Site{
		log:        util.NewLogger("site"),
		loadpoints: []*Loadpoint{high, low},
	}
	site.initLoadpointCoordination(30 * time.Second)

	// An idle lower-priority loadpoint reserves only the higher loadpoint's
	// incremental startup demand. Existing physical base load is not claimed again.
	if got, want := site.reservedPVPower(low), high.EffectiveMinPower()-high.chargePower; got != want {
		t.Errorf("low: want %.0f, got %.0f", want, got)
	}
	if high.EffectiveMinPower() == high.EffectiveMaxPower() {
		t.Fatal("test requires min and max power to differ")
	}

	// a timer restarting on every surplus dip must not reserve at all (#32778)
	high.pvTimer = clock.NewMock().Now()
	if got := site.reservedPVPower(low); got != 0 {
		t.Errorf("low while high timer restarts: want 0, got %.0f", got)
	}
	high.pvTimer = pvTimerStarted(10 * time.Minute)

	if got := site.reservedPVPower(high); got != 0 {
		t.Errorf("high: want 0, got %.0f", got)
	}

	// A charging lower-priority loadpoint keeps its allocation during the long
	// qualification period.
	low.status = api.StatusC
	low.enabled = true
	if got := site.reservedPVPower(low); got != 0 {
		t.Errorf("charging low during qualification: want 0, got %.0f", got)
	}

	// One round before activation, the handover claim is staged.
	high.clock.(*clock.Mock).Add(19 * time.Minute)
	if got, want := site.reservedPVPower(low), high.EffectiveMinPower()-high.chargePower; got != want {
		t.Errorf("charging low during handover: want %.0f, got %.0f", want, got)
	}

	// A timer reset releases the staged handover immediately.
	high.pvTimer = time.Time{}
	if got := site.reservedPVPower(low); got != 0 {
		t.Errorf("low after timer reset: want 0, got %.0f", got)
	}

	// A command already issued to the higher-priority loadpoint reserves only its
	// unacknowledged incremental demand.
	now := high.clock.Now()
	if _, err := site.prepareLoadpointDemand(high, 2760, high.chargePower, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	site.commitLoadpointDemand(high)
	if got, want := site.reservedPVPower(low), 2760-high.chargePower; got != want {
		t.Errorf("pending command: want %.0f, got %.0f", want, got)
	}

	// Invalid/stale data cannot acknowledge the pending command.
	if err := site.observeLoadpointDemand(high, 2760, false, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got, want := site.reservedPVPower(low), 2760-high.chargePower; got != want {
		t.Errorf("invalid acknowledgement: want %.0f, got %.0f", want, got)
	}

	// Fresh partial and complete measurements reduce and release the claim.
	if err := site.observeLoadpointDemand(high, 1500, true, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got, want := site.reservedPVPower(low), 1260.0; got != want {
		t.Errorf("partial acknowledgement: want %.0f, got %.0f", want, got)
	}
	if err := site.observeLoadpointDemand(high, 2760, true, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := site.reservedPVPower(low); got != 0 {
		t.Errorf("complete acknowledgement: want 0, got %.0f", got)
	}

	requireDeadline := now.Add(5 * time.Second)
	if _, err := site.prepareLoadpointDemand(high, 2760, 0, now, requireDeadline); err != nil {
		t.Fatal(err)
	}
	site.commitLoadpointDemand(high)
	high.clock.(*clock.Mock).Set(requireDeadline.Add(time.Second))
	if got := site.reservedPVPower(low); got != 0 {
		t.Errorf("expired command: want 0, got %.0f", got)
	}
}

func TestLoadpointCoordinationStartupOrder(t *testing.T) {
	low := newPVLoadpoint(0, api.ModePV, api.StatusB, false, time.Time{})
	high := newPVLoadpoint(2, api.ModePV, api.StatusB, false, time.Time{})
	mid := newPVLoadpoint(1, api.ModePV, api.StatusB, false, time.Time{})
	equal := newPVLoadpoint(1, api.ModePV, api.StatusB, false, time.Time{})

	site := &Site{loadpoints: []*Loadpoint{low, mid, equal, high}}
	got := site.orderedStartupLoadpoints()
	want := []*Loadpoint{high, mid, equal, low}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position %d: want priority %d, got %d", i, want[i].EffectivePriority(), got[i].EffectivePriority())
		}
	}
}

func TestColdStartPriorityIntentBeforeLowerEnable(t *testing.T) {
	Voltage = 230

	high := newPVLoadpoint(1, api.ModePV, api.StatusB, false, time.Time{})
	high.phases = 3
	high.chargePower = 167
	high.demandPower = 167
	high.demandValid = true
	high.Enable.Threshold = -3800
	high.Enable.Delay = 20 * time.Minute

	low := newPVLoadpoint(0, api.ModePV, api.StatusB, false, time.Time{})

	site := &Site{
		log:        util.NewLogger("site"),
		loadpoints: []*Loadpoint{low, high}, // deliberately lower priority first
	}
	site.initLoadpointCoordination(30 * time.Second)

	ordered := site.orderedStartupLoadpoints()
	if ordered[0] != high {
		t.Fatal("higher-priority loadpoint must be assessed first")
	}

	if got := high.pvMaxCurrent(api.ModePV, -4000, 0, false, false); got != 0 {
		t.Fatalf("higher qualification: want 0A while timer runs, got %.1fA", got)
	}
	if !high.PvChargeStarting() {
		t.Fatal("higher-priority startup intent was not established")
	}

	withoutReservation := newPVLoadpoint(0, api.ModePV, api.StatusB, false, time.Time{})
	if got := withoutReservation.pvMaxCurrent(api.ModePV, -3202, 0, false, false); got != minA {
		t.Fatalf("lower without reservation: want %.1fA, got %.1fA", minA, got)
	}

	sitePower := -3202 + site.reservedPVPower(low)
	if got := low.pvMaxCurrent(api.ModePV, sitePower, 0, false, false); got != 0 {
		t.Fatalf("lower with higher intent: want 0A, got %.1fA", got)
	}
}

func TestLoadpointPriorityStateReevaluation(t *testing.T) {
	now := clock.NewMock().Now()
	high := newPVLoadpoint(1, api.ModePV, api.StatusB, false, time.Time{})
	high.Enable.Delay = 20 * time.Minute
	low := newPVLoadpoint(0, api.ModePV, api.StatusC, true, time.Time{})
	equal := newPVLoadpoint(1, api.ModePV, api.StatusC, true, time.Time{})

	site := &Site{loadpoints: []*Loadpoint{low, equal, high}}
	site.initLoadpointCoordination(30 * time.Second)
	site.pvPriorityStates[high] = pvPriorityInactive

	high.pvTimer = now
	site.updatePVPriorityState(high)
	if got := site.nextLoadpointReevaluation(); got != low {
		t.Fatalf("timer start: want lower-priority loadpoint, got %p", got)
	}
	if got := site.nextLoadpointReevaluation(); got != nil {
		t.Fatalf("equal priority must not be scheduled, got %p", got)
	}

	deadline := site.nextCoordinationDeadline()
	high.clock.(*clock.Mock).Set(deadline)
	if err := site.processCoordinationDeadlines(deadline); err != nil {
		t.Fatal(err)
	}
	if got := site.nextLoadpointReevaluation(); got != low {
		t.Fatalf("handover deadline: want lower-priority loadpoint, got %p", got)
	}

	high.pvTimer = time.Time{}
	site.updatePVPriorityState(high)
	if got := site.nextLoadpointReevaluation(); got != low {
		t.Fatalf("timer reset: want lower-priority loadpoint, got %p", got)
	}
}

func TestDefaultEnableDelayHasQualificationStage(t *testing.T) {
	now := clock.NewMock().Now()
	high := newPVLoadpoint(1, api.ModePV, api.StatusB, false, now)
	high.Enable.Delay = time.Minute
	low := newPVLoadpoint(0, api.ModePV, api.StatusC, true, time.Time{})
	low.clock = high.clock
	site := &Site{log: util.NewLogger("site"), loadpoints: []*Loadpoint{high, low}}
	site.initLoadpointCoordination(30 * time.Second)

	if got := high.pvPriorityState(site.loadpointHandoverWindow()); got != pvPriorityQualifying {
		t.Fatalf("timer start: want qualifying, got %d", got)
	}
	if got := site.reservedPVPower(low); got != 0 {
		t.Fatalf("charging low during default qualification: want 0, got %.0fW", got)
	}

	high.clock.(*clock.Mock).Add(30 * time.Second)
	if got := high.pvPriorityState(site.loadpointHandoverWindow()); got != pvPriorityHandover {
		t.Fatalf("timer midpoint: want handover, got %d", got)
	}
	if got := site.reservedPVPower(low); got != high.EffectiveMinPower() {
		t.Fatalf("charging low during default handover: want %.0fW, got %.0fW", high.EffectiveMinPower(), got)
	}
}

var _ loadpoint.API = (*Loadpoint)(nil)
