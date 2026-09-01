# Core Domain: Site, Loadpoint, and the Control Loop

## Object Hierarchy

```
Site (orchestrator — core/site.go)
├── Meters: Grid, PV[], Battery[], Auxiliary[], External[]
├── Tariffs: Grid, FeedIn, CO2, Solar
├── Coordinator (vehicle <-> loadpoint assignment)
├── Prioritizer (power allocation fairness)
└── Loadpoints[] (core/loadpoint.go)
    ├── Charger      (api.Charger — hardware controller)
    ├── Vehicle      (api.Vehicle — EV battery state via cloud API)
    ├── ChargeMeter  (api.Meter — AC power at charger)
    └── Circuit      (optional — electrical domain limits)
```

## Key Interfaces (api/api.go)

### Meter
- `Meter` — `CurrentPower() (float64, error)` — watts
- `MeterEnergy` — `TotalEnergy() (float64, error)` — kWh
- `PhaseCurrents` / `PhaseVoltages` / `PhasePowers` — per-phase readings

### Battery
- `Battery` — `Soc() (float64, error)` — 0-100%
- `BatteryCapacity` — kWh
- `BatteryController` — set charge/discharge/hold mode

### Charger
- `Charger` — `Status()`, `Enabled()`, `Enable(bool)`, `MaxCurrent(int64)`
- `ChargerEx` — milliamp-precision current via `MaxCurrentMillis(float64)`
- `PhaseSwitcher` — `Phases1p3p(int) error`
- `ChargeRater` — `ChargedEnergy() (float64, error)`
- `ChargeTimer` — `ChargeDuration() (time.Duration, error)`

### Vehicle
- `Vehicle` — `Soc()`, `Capacity()`, `Identifiers()`, `Phases()`, `OnIdentified()`
- `VehicleRange`, `VehicleOdometer`, `VehicleClimater`, `VehicleFinishTimer`, `VehiclePosition`
- `ChargeController` — remote start/stop on vehicle
- `CurrentLimiter` — `GetMinMaxCurrent()` for vehicle-side current limits
- `CurrentController` — some vehicles (Tesla, Fiat) also implement `MaxCurrent()` to set charge current from the vehicle side

## Charge Modes

| Mode | Behavior |
|------|----------|
| `OFF` | Disabled (unless welcome charge) |
| `NOW` | Max current immediately |
| `MINPV` | Min current when PV surplus; fast if cheap tariff |
| `PV` | Ramp current proportional to available solar |

## Charge States (IEC 61851)

- `A` — not connected
- `B` — connected, not charging
- `C` — connected, charging

## The Control Loop (Site.update — runs every N seconds)

```
1. Update all meters (grid, PV, battery, aux)
2. For each loadpoint: UpdateChargePowerAndCurrents()
3. Calculate site power balance:
   sitePower = gridPower + batteryPower + excessDCPower
             + residualPower - auxPower - flexiblePower
4. Apply battery priority rules (prioritySoc, bufferSoc)
5. Get tariff rates
6. Update the selected loadpoint: Update(sitePower, ...)
   ├── Read charger status
   ├── Detect/identify vehicle
   ├── Check plan requirements (minSOC, target time)
   ├── Check limits (limitSOC, limitEnergy)
   ├── MODE switch -> calculate target current
   ├── Cap at maxCurrent, respect circuit limits
   ├── Send MaxCurrent() to charger
   └── Record metrics
7. Push updates to WebSocket + metrics
```

The normal loop re-reads every loadpoint's measured power, then updates one
loadpoint in round-robin order. Cold startup first updates all loadpoints in
descending effective-priority order. This establishes higher-priority PV intent
before a lower-priority loadpoint can enable, independent of configuration order.

Higher-priority PV enable timers reserve only incremental startup demand:
effective minimum power minus already measured base consumption. An idle
lower-priority loadpoint respects that claim for the full qualification timer.
An already-charging lower-priority loadpoint keeps its allocation until the
higher-priority timer enters a handover window. That window is at most one
round-trip and never more than half the qualification delay, so the default
one-minute timer retains a qualification stage. Timer starts, handover
transitions, resets, and completed load commands enqueue immediate, deduplicated
reevaluation of affected lower-priority PV loadpoints. An internal deadline wake
triggers handover and pending-command expiry without waiting for round-robin.
Material reductions of an active command claim also enqueue reevaluation so
released headroom becomes usable as measured load appears. Preparing or
replacing a claim does not enqueue another full site update; battery feed-forward
already establishes the immediate electrical headroom.

Current, enable, and phase-switch writes prepare that feed-forward claim before
the actuator call. Phase switching computes the new target from the requested
effective phases and offered current. A following current write replaces the
phase claim with the combined target, while a failed phase write restores the
previous claim. Once unacknowledged demand expires, later current writes claim
only growth beyond the previous successful target instead of repeatedly
reserving the full idle target. Lower-priority PV allocation uses the same
incremental reservation. Latent demand below the covered target is handled by
the closed-loop grid response if it appears after expiry.

## PV Surplus Charging (pvMaxCurrent in core/loadpoint.go)

```
1. Read effective min/max current limits
2. Reduce sitePower by battery boost power
3. Consider phase switching (1p <-> 3p) if supported
4. deltaCurrent = powerToCurrent(-sitePower, activePhases)
   targetCurrent = effectiveCurrent + deltaCurrent
5. Below minCurrent -> start disable timer (default 3 min)
6. Surplus returns -> start enable timer (default 1 min)
7. Cap at maxCurrent
```

## Battery Priority Rules

| Setting | Effect |
|---------|--------|
| `prioritySoc` | Below this: battery charges first, EV gets 0 |
| `bufferSoc` | Above this: EV charging may use the home battery |
| `bufferStartSoc` | Above this: EV charging can begin even if importing |

## Effective Price Calculation

```
greenShare = (max(pvPower,0) + max(batteryPower,0)) / totalChargePower
effectivePrice = gridPrice * (1 - greenShare) + feedInPrice * greenShare
```

## Concurrency Model

- **Site** owns `RWMutex` for its state (meters, battery, tariffs)
- **Loadpoint** owns `RWMutex` for its state (charger, vehicle, current)
- **Coordinator** owns `RWMutex` for vehicle <-> loadpoint tracking
- No global locks — ordering prevents deadlocks

### Channels

| Channel | Scope | Buffer | Purpose |
|---------|-------|--------|---------|
| `valueChan` | Site | Unbounded (`chanx.NewUnboundedChan`) | State changes -> DB + UI (ordering) |
| `lpUpdateChan` | Site | 1 | Early loadpoint update requests |
| priority reevaluation queue | Site | Deduplicated | Serialized updates after a higher-priority claim changes |
| `pushChan` | Loadpoint | 16 | User notifications, queued via `valueChan` so the message renders the state at event time |

## Tariff Integration

Types: `TariffUsageGrid`, `TariffUsageFeedIn`, `TariffUsageCo2`, `TariffUsagePlanner`, `TariffUsageSolar`

### Smart Features
- **Cheap-tariff override** — rate below threshold -> fast charge
- **Smart feed-in** — feed-in rate above threshold -> prioritize export
- **Planner** (`core/planner/planner.go`) — finds cheapest time slots for target SOC/energy by deadline
  - `optimalPlan()` — cheapest non-contiguous slots
  - `continuousPlan()` — cheapest continuous window (fallback)

## Key File Locations

- `api/api.go` — all core interfaces
- `core/site.go` — Site orchestrator + control loop
- `core/loadpoint.go` — Loadpoint state machine (pvMaxCurrent, mode switch)
- `core/site_battery.go` — battery priority logic
- `core/site_tariffs.go` — tariff integration
- `core/planner/planner.go` — charge time optimization
- `core/prioritizer/prioritizer.go` — power allocation across loadpoints
- `core/circuit/circuit.go` — electrical domain limits
