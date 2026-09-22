package core

import (
	"testing"
	"time"

	evbus "github.com/asaskevich/EventBus"
	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/util"
)

func newPVLoadpoint(prio int, mode api.ChargeMode, status api.ChargeStatus, enabled bool, timer time.Time) *Loadpoint {
	return &Loadpoint{
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
}

func TestPvChargeStarting(t *testing.T) {
	now := clock.NewMock().Now()

	// enable timer running but car already full (soc at default 100% limit): not starting up
	enablePendingFull := newPVLoadpoint(0, api.ModeSmart, api.StatusB, false, now)
	enablePendingFull.vehicleSoc = 100

	tc := []struct {
		name     string
		lp       *Loadpoint
		starting bool
	}{
		{"enable timer running", newPVLoadpoint(0, api.ModeSmart, api.StatusB, false, now), true},
		{"enabled not charging", newPVLoadpoint(0, api.ModeSmart, api.StatusB, true, time.Time{}), false},
		{"enabled and charging", newPVLoadpoint(0, api.ModeSmart, api.StatusC, true, time.Time{}), false},
		{"disabled idle", newPVLoadpoint(0, api.ModeSmart, api.StatusB, false, time.Time{}), false},
		{"disconnected", newPVLoadpoint(0, api.ModeSmart, api.StatusA, false, now), false},
		{"not pv mode", newPVLoadpoint(0, api.ModeNow, api.StatusB, false, now), false},
		{"enable pending but car full", enablePendingFull, false},
		// elapsed means a delay was skipped, e.g. by a feed-in pause, not an enable pending
		{"timer elapsed", newPVLoadpoint(0, api.ModeSmart, api.StatusB, false, elapsed), false},
	}

	for _, tc := range tc {
		if got := tc.lp.PvChargeStarting(); got != tc.starting {
			t.Errorf("%s: want %v, got %v", tc.name, tc.starting, got)
		}
	}
}

func TestReservedPVPower(t *testing.T) {
	Voltage = 230

	// higher-priority loadpoint (prio 1) starting up
	high := newPVLoadpoint(1, api.ModeSmart, api.StatusB, false, clock.NewMock().Now())
	// lower-priority loadpoint (prio 0) in PV mode
	low := newPVLoadpoint(0, api.ModeSmart, api.StatusB, false, time.Time{})

	site := &Site{
		log:        util.NewLogger("site"),
		loadpoints: []*Loadpoint{high, low},
	}

	// low reserves high's anticipated max power while high is starting up
	if got, want := site.reservedPVPower(low), high.EffectiveMaxPower(); got != want {
		t.Errorf("low: want %.0f, got %.0f", want, got)
	}

	// high (top priority) reserves nothing
	if got := site.reservedPVPower(high); got != 0 {
		t.Errorf("high: want 0, got %.0f", got)
	}

	// once high is charging it no longer reserves surplus from low
	high.status = api.StatusC
	high.enabled = true
	high.pvTimer = time.Time{}
	if got := site.reservedPVPower(low); got != 0 {
		t.Errorf("low after high charging: want 0, got %.0f", got)
	}

	// high stays enabled and connected but no longer draws (car full): no reservation (#31684)
	high.status = api.StatusB
	if got := site.reservedPVPower(low); got != 0 {
		t.Errorf("low after high stopped drawing: want 0, got %.0f", got)
	}
}

func TestReservedPVPowerWithLoadpointDemandCoordination(t *testing.T) {
	Voltage = 230

	// higher-priority loadpoint (prio 1) starting up
	high := newPVLoadpoint(1, api.ModeSmart, api.StatusB, false, clock.NewMock().Now())
	high.Enable.Delay = 20 * time.Minute
	high.chargePower = 167
	high.demandPower = 167
	high.demandValid = true
	// lower-priority loadpoint (prio 0) in smart mode
	low := newPVLoadpoint(0, api.ModeSmart, api.StatusB, false, time.Time{})
	low.clock = high.clock

	site := &Site{
		log:                   util.NewLogger("site"),
		loadpoints:            []*Loadpoint{high, low},
		batteryPowerRegulator: coordinationTestRegulator(),
	}
	site.initLoadpointCoordination(30 * time.Second)

	// An idle lower-priority loadpoint reserves only the higher loadpoint's
	// incremental startup demand. Existing physical base load is not claimed again.
	if got, want := site.reservedPVPower(low), high.EffectiveMaxPower()-high.chargePower; got != want {
		t.Errorf("low: want %.0f, got %.0f", want, got)
	}

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
	if got, want := site.reservedPVPower(low), high.EffectiveMaxPower()-high.chargePower; got != want {
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
	low := newPVLoadpoint(0, api.ModeSmart, api.StatusB, false, time.Time{})
	high := newPVLoadpoint(2, api.ModeSmart, api.StatusB, false, time.Time{})
	mid := newPVLoadpoint(1, api.ModeSmart, api.StatusB, false, time.Time{})
	equal := newPVLoadpoint(1, api.ModeSmart, api.StatusB, false, time.Time{})

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

	high := newPVLoadpoint(1, api.ModeSmart, api.StatusB, false, time.Time{})
	high.phases = 3
	high.chargePower = 167
	high.demandPower = 167
	high.demandValid = true
	high.Enable.Threshold = -3800
	high.Enable.Delay = 20 * time.Minute

	low := newPVLoadpoint(0, api.ModeSmart, api.StatusB, false, time.Time{})

	site := &Site{
		log:                   util.NewLogger("site"),
		loadpoints:            []*Loadpoint{low, high}, // deliberately lower priority first
		batteryPowerRegulator: coordinationTestRegulator(),
	}
	site.initLoadpointCoordination(30 * time.Second)

	ordered := site.orderedStartupLoadpoints()
	if ordered[0] != high {
		t.Fatal("higher-priority loadpoint must be assessed first")
	}

	if got := high.pvMaxCurrent(-4000, 0, false, false); got != 0 {
		t.Fatalf("higher qualification: want 0A while timer runs, got %.1fA", got)
	}
	if !high.PvChargeStarting() {
		t.Fatal("higher-priority startup intent was not established")
	}

	withoutReservation := newPVLoadpoint(0, api.ModeSmart, api.StatusB, false, time.Time{})
	withoutReservation.Enable.Delay = 0
	if got := withoutReservation.pvMaxCurrent(-3202, 0, false, false); got != minA {
		t.Fatalf("lower without reservation: want %.1fA, got %.1fA", minA, got)
	}

	low.Enable.Delay = 0
	sitePower := -3202 + site.reservedPVPower(low)
	if got := low.pvMaxCurrent(sitePower, 0, false, false); got != 0 {
		t.Fatalf("lower with higher intent: want 0A, got %.1fA", got)
	}
}

func TestLoadpointPriorityStateReevaluation(t *testing.T) {
	now := clock.NewMock().Now()
	high := newPVLoadpoint(1, api.ModeSmart, api.StatusB, false, time.Time{})
	high.Enable.Delay = 20 * time.Minute
	low := newPVLoadpoint(0, api.ModeSmart, api.StatusC, true, time.Time{})
	equal := newPVLoadpoint(1, api.ModeSmart, api.StatusC, true, time.Time{})

	site := &Site{loadpoints: []*Loadpoint{low, equal, high}, batteryPowerRegulator: coordinationTestRegulator()}
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
	high := newPVLoadpoint(1, api.ModeSmart, api.StatusB, false, now)
	high.Enable.Delay = time.Minute
	low := newPVLoadpoint(0, api.ModeSmart, api.StatusC, true, time.Time{})
	low.clock = high.clock
	site := &Site{log: util.NewLogger("site"), loadpoints: []*Loadpoint{high, low}, batteryPowerRegulator: coordinationTestRegulator()}
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
	if got := site.reservedPVPower(low); got != high.EffectiveMaxPower() {
		t.Fatalf("charging low during default handover: want %.0fW, got %.0fW", high.EffectiveMaxPower(), got)
	}
}

// feedInCharger is a stateful charger, unlike the mocks used elsewhere, since the
// feed-in pause is asserted across multiple update cycles
type feedInCharger struct {
	status  api.ChargeStatus
	enabled bool
}

func (c *feedInCharger) Status() (api.ChargeStatus, error) { return c.status, nil }
func (c *feedInCharger) Enabled() (bool, error)            { return c.enabled, nil }
func (c *feedInCharger) MaxCurrent(int64) error            { return nil }

func (c *feedInCharger) Enable(v bool) error {
	c.enabled = v
	// a disabled charger stops drawing, i.e. reports connected instead of charging
	if !v && c.status == api.StatusC {
		c.status = api.StatusB
	}
	return nil
}

// TestReservedPVPowerSmartFeedInPause asserts that a loadpoint paused by its smart feed-in
// priority limit does not reserve surplus: it is meant to export instead of charge
func TestReservedPVPowerSmartFeedInPause(t *testing.T) {
	// the feed-in rate rises above the limit while the loadpoint is idle or charging
	for _, tc := range []struct {
		name    string
		status  api.ChargeStatus
		enabled bool
	}{
		{"idle", api.StatusB, false},
		{"charging", api.StatusC, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clck := clock.NewMock()
			limit := 0.05

			car := newPVLoadpoint(1, api.ModeSmart, tc.status, tc.enabled, time.Time{})
			car.bus = evbus.New()
			car.clock = clck
			car.charger = &feedInCharger{status: tc.status, enabled: tc.enabled}
			car.chargeMeter = newChargeMeter(&Null{}) // silence nil panics
			car.chargeRater = &Null{}                 // silence nil panics
			car.chargeTimer = &Null{}                 // silence nil panics
			car.wakeUpTimer = NewTimer()
			car.smartFeedInPriorityLimit = &limit
			car.vehicleSoc = 20
			car.limitSoc = 80
			attachListeners(t, car)

			low := newPVLoadpoint(0, api.ModeSmart, api.StatusB, false, time.Time{})

			site := &Site{
				log:        util.NewLogger("site"),
				loadpoints: []*Loadpoint{car, low},
			}

			// checkSmartLimit looks up rates by wall clock time, not lp.clock
			now := time.Now()
			feedin := api.Rates{{Start: now.Add(-time.Hour), End: now.Add(time.Hour), Value: limit + 0.01}}

			// the pause holds across cycles, so must the absence of a reservation
			for i := range 2 {
				car.Update(-3500, 0, nil, feedin, false, false, 0, nil, nil, nil)

				if car.enabled {
					t.Fatalf("cycle %d: car must be paused by the feed-in limit", i)
				}

				if got := site.reservedPVPower(low); got != 0 {
					t.Errorf("cycle %d: paused car reserves %.0fW, want 0W (pvTimer %v)", i, got, car.pvTimer)
				}

				clck.Add(time.Minute)
			}
		})
	}
}

var _ loadpoint.API = (*Loadpoint)(nil)
