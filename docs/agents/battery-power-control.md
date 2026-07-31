# Reliable battery power regulation

## Status

Implementation design for a small, stateful battery regulator.

The initial target is one Huawei SUN2000 continuous battery controller using a
Shelly Pro 3EM grid meter and Huawei battery feedback through a Modbus proxy.
The generic `api.BatteryPowerController` actuator interface remains reusable,
but coordination of multiple continuous battery controllers is intentionally
out of scope for the first release.

## Decision summary

- Run one non-overlapping battery control loop every 5 seconds.
- Offset periodic regulator ticks by 2.5 seconds from the site loop phase.
- Keep the normal evcc site interval at its 30 second default.
- Support exactly one active continuous battery power controller per site.
- Read fresh grid and battery power during every normal control cycle.
- Use the raw directional grid error without an error filter or PID state.
- Keep a 100 W deadband for starting from neutral and use a tighter 50 W
  deadband while a direction is active.
- Bound every command magnitude increase to 1500 W per cycle.
- Require battery acknowledgement before another magnitude increase.
- After reducing a nonzero command, wait for battery feedback to reach the
  reduced magnitude before increasing again. Further reductions remain
  immediate.
- Reduce an unsafe command immediately. A reduction may reach zero in one
  cycle, but it must never cross zero.
- Always write exact zero. Never suppress zero because of the write threshold.
- Acquire ownership, reverse direction, and recover from faults only through a
  successful zero command followed by a later near-zero battery sample.
- Preserve half the configured site residual power while charging from PV.
- Center active discharge control at 20 W grid export so its 50 W deadband
  accepts grid power from 70 W export to 30 W import.
- Stop on invalid grid data, battery feedback unavailable for 15
  seconds, stale policy, write failure, failed mode handoff, release, or
  shutdown.
- During the battery feedback grace period, keep the active command or reduce
  it from fresh grid data. Never increase, start, reverse, or refresh it.
- Avoid same-cycle retries except for the best-effort zero after a failed write.

The regulator deliberately adds battery effort carefully and removes battery
effort quickly. This protects against delayed Huawei response without hiding
large household or PV changes behind additional filtering.

## Why state is still required

A stateless equation is not safe for this actuator:

- Huawei may take several control cycles to show a setpoint change.
- Repeating full corrections before the inverter responds causes oscillation.
- Charge and discharge use different registers and watchdog ownership.
- A direct sign reversal can start the new direction while the old direction is
  still physically active.
- A process restart may find a forced Huawei command that is still active for up
  to one minute.
- Discrete `BatteryController` modes and continuous power control must never own
  the inverter simultaneously.

The required state is limited to:

```go
type ControlState struct {
    Phase                 Phase
    AppliedCommand        float64
    Initialized           bool
    PendingCommand        *PendingCommand
    NeutralSince          time.Time
    NeutralRequired       bool
    LastBatterySample     Sample
    LastWriteAt           time.Time
    Policy                ControlPolicy
    ChargeBlockedUntil    time.Time
    DischargeBlockedUntil time.Time
}
```

There is no filtered error, weak-start counter, accumulated integral, or
multi-battery allocation state.

## Component responsibilities

### Site policy

The normal site loop owns slow policy:

- current battery mode;
- HEMS dimming;
- tariff-based discharge control;
- site residual power;
- charge and discharge power limits;
- SoC-derived `chargeAllowed` and `dischargeAllowed`;
- policy timestamp.

The 5 second regulator does not read SoC.

Continuous control requires both power limits and SoC limits on the controlled
battery. Missing power or SoC limits keep the policy inactive.

The site keeps the last successful per-device SoC for up to 60 seconds. One
transient SoC read failure therefore does not stop and restart regulation, while
prolonged SoC loss still disables both directions.

SoC eligibility uses the exact configured limits. Charging remains allowed while
SoC is below the configured maximum, and discharging remains allowed while SoC
is above the configured minimum. The regulator does not add headroom that could
prevent a battery from reaching its configured maximum.

### Battery regulator

The regulator:

- owns all continuous signed power commands;
- reads the grid and controlled battery meter;
- applies deadband, gain, and step limiting;
- gates increases on actuator acknowledgement;
- enforces startup and reversal neutrality;
- releases ownership before discrete mode control.

### Existing battery power controller adapter

The existing meter-side `batteryPowerController` is not legacy. It is the
actuator boundary below the regulator:

- negative signed commands call the configured charge setter;
- positive signed commands call the configured discharge setter;
- zero cancels the active direction watchdog;
- a configured generic stop/release sequence runs after direction stop;
- when the direction is unknown after startup or a failed power write, zero
  invokes both direction setters before the generic stop sequence;
- a failed release keeps internal direction state so zero can be retried.

The regulator prevents normal sign reversal. The adapter still protects direct
callers by stopping its known previous direction before invoking the opposite
setter, but it cannot prove physical neutrality by itself.

### Discrete battery mode controller

`api.BatteryController` remains responsible for modes such as Normal, Hold, and
grid Charge.

Before a discrete mode write:

1. release continuous power control;
2. wait for the release call to finish;
3. write the discrete mode;
4. publish a new continuous policy only if the mode transition succeeded.

If any mode write fails, continuous control remains released. The site loop
retries the requested mode, or explicitly restores Normal when the original
request has disappeared, before continuous control may resume.

## Scope

The first release accepts exactly one `api.BatteryPowerController`.

If more than one is configured:

- the regulator is not created;
- an error is logged;
- the fallback zero path releases every controller after initial meter setup;
- a failed fallback release is retried on later site updates until confirmed;
- no continuous nonzero command is issued.

This removes unsafe aggregate acknowledgement and false neutral detection where
opposite battery powers could cancel numerically. Multiple-controller
coordination requires per-battery command, acknowledgement, and neutral state
and should be designed separately.

## Sign convention and targets

| Value | Meaning |
|---|---|
| Positive grid power | Grid import |
| Negative grid power | Grid export |
| Positive battery command/power | Battery discharge |
| Negative battery command/power | Battery charge |
| Zero battery command | Stop continuous control |

Directional grid targets:

```text
chargeGridTarget     = -residualPower / 2
dischargeGridTarget  = -20 W
error                = gridPower - activeGridTarget
```

Charging therefore retains half the residual export margin used for PV charging
at a loadpoint. Discharging uses a biased grid deadband from -70 W to +30 W.
This favors a small export while tolerating up to 30 W import to preserve the
existing write threshold and control damping.

From neutral:

```text
chargeStartTarget    = min(chargeGridTarget, 0 W)
dischargeStartTarget = max(dischargeGridTarget, 0 W)

gridPower < chargeStartTarget - startDeadband    -> request charge
gridPower > dischargeStartTarget + startDeadband -> request discharge
otherwise                                       -> remain neutral
```

A negative residual power may allow half the configured import during
established PV charging, but it must not start charging from neutral solely
because the site is importing. The 100 W startup deadband prevents cycling
between neutral and a small command. Once charging or discharging, the 50 W
active deadband permits finer grid tracking without weakening startup
hysteresis. The discharge bias does not wake a neutral battery for imports at or
below 100 W.

At the nominal -20 W center, continuous export is 0.16 kWh over 8 hours or
0.24 kWh over 12 hours. The accepted discharge band can export up to 70 W.
This is an intentional self-consumption tradeoff for avoiding steady low grid
import and does not compensate for large load changes between control ticks.
Sites that prohibit grid export must not use this tuning.

## Scheduler and sample validity

- One regulator goroutine per site.
- One synchronous `tick` at a time.
- No queued catch-up cycles.
- Run one immediate tick, then start the five-second cadence at a 2.5-second
  offset from the site loop. This avoids predictable collisions every 30
  seconds when both loops access shared Huawei Modbus paths.
- Stop closes the scheduler, releases immediately, then joins the worker. This
  lets zero bypass a worker blocked in a meter read.
- Read Huawei battery feedback first, then read the fast Shelly grid meter.
  This keeps the grid input fresh even when Huawei takes several seconds.
- Normal command decisions use both fresh samples. Feedback grace uses the
  fresh grid sample only to hold or reduce the existing command.

Each sample records:

```go
type Sample struct {
    Value      float64
    StartedAt  time.Time
    FinishedAt time.Time
    Err        error
}
```

A sample is invalid when:

- the read failed;
- the value is `NaN` or infinite;
- a grid read took longer than 4 seconds;
- the completed grid sample is older than one 5 second control interval when
  the command decision is made.

The meter API cannot cancel an in-flight read. A cycle that blocks cannot
overlap another cycle. Huawei's watchdog and one-minute forced period remain the
out-of-band safety limits if an I/O call does not return.

A successful Huawei read is accepted regardless of its duration. Its completion
timestamp represents the fresh feedback point, and the Shelly grid read happens
afterward. Rejecting successful 4.9-9.1 second responses would only force a
stop after current feedback had already arrived.

Every cycle with valid grid and battery samples writes one DEBUG snapshot before
acknowledgement or control decisions. The stable `cycle=<n>` record includes the
phase, fresh grid and battery power, applied and pending commands, active target
and raw error, selected demand direction, direction availability, force-charge
and neutral state, command ages, last command action, stop retry timing, and
sample read timing. A raw-grid safety retreat includes the same cycle ID in its
existing command action log so delayed Huawei feedback, the Shelly sample, and
the resulting retreat can be correlated without another action record.

### Battery feedback grace

An active charging or discharging command may continue for up to 15 seconds
from the last valid battery sample when the next battery read fails, is invalid,
or returns stale timestamps.

Grace requires:

- an existing nonzero charging or discharging command;
- a previously valid battery sample;
- fresh valid grid data read after the battery attempt;
- fresh policy that still permits the active direction.

During grace:

- no magnitude increase, startup, reversal, acknowledgement, or periodic
  command refresh is allowed;
- fresh grid data may reduce the command immediately toward zero;
- force charge may hold but not increase;
- a grid failure, policy invalidation, active-direction eligibility or limit
  change, write failure, release, or shutdown still commands zero immediately;
- expiry commands zero and enters normal fault recovery.

Fifteen seconds permits one recovery attempt after a failed read that begins
five seconds after the previous sample and itself takes about five seconds. A
10-second limit would commonly expire on that first failure. The limit is
evaluated when feedback remains invalid. A newly valid response ends degraded
operation even if the non-cancelable read itself took longer.

## Controller phases

- `released`: continuous control does not own the battery;
- `neutral`: command is zero, possibly awaiting a post-stop neutral sample;
- `charging`;
- `discharging`;
- `faultStopping`: only stop and neutral recovery are allowed.

`PendingCommand` represents an unacknowledged changed nonzero command:

```go
type PendingCommand struct {
    PreviousCommand float64
    Command         float64
    BaselinePower   float64
    AppliedAt       time.Time
}
```

`AppliedCommand` changes only after a successful actuator call or a successful
best-effort zero following a failed command.

## Ownership acquisition

The regulator never assumes that a released or newly constructed actuator is
physically neutral.

When a valid active policy acquires control:

1. call `SetBatteryPower(0)`;
2. record the successful zero timestamp;
3. require a later fresh battery sample within the neutral tolerance;
4. only then allow a nonzero command.

This covers process restart while Huawei's previous forced command is still
inside its one-minute expiry.

## Control algorithm

### 1. Safety checks

Every cycle first validates:

- current ownership;
- fresh grid sample;
- fresh policy;
- fresh battery sample, or an eligible active command inside feedback grace;
- active direction still allowed by policy.

Invalid grid data, stale policy, ineligible feedback, and write failures attempt
zero and enter `faultStopping`. An active direction becoming disallowed is an
intentional stop to `neutral`, not a fault.

### 2. Acknowledgement

After any changed nonzero command, no further increase is allowed until a later
battery sample acknowledges the command.

For a magnitude increase, acknowledgement succeeds when either:

- measured battery power reaches the command direction within
  `min(250 W, 50% of command delta)`; or
- battery power moved toward the command by at least
  `max(10 W, 25% of command delta)`.

For a reduction, acknowledgement requires feedback to reach the safer side of
the new command within `min(250 W, 50% of command delta)`:

```text
charging:    measuredPower >= command - adaptiveTolerance
discharging: measuredPower <= command + adaptiveTolerance
```

Partial movement does not acknowledge a reduction because it would permit the
next increase while Huawei is still applying the old, stronger command.
Scaling tolerance with command delta prevents unchanged feedback from
acknowledging a 25-50 W precision correction. The 10 W movement floor remains
large enough to reject unchanged feedback while allowing observable movement
from a 25 W setpoint change.

Grid movement cannot acknowledge a battery command because PV and household
load may change independently.

If the 30 second acknowledgement window expires after grid demand for a pending
magnitude increase has returned to the active deadband, the regulator abandons
the increase and writes `PreviousCommand`, the last acknowledged command. It
does not roll back earlier because grid and battery feedback may arrive with
different delays. This rollback is not an acknowledgement and does not create
or clear directional cooldown history. A nonzero previous command becomes a
normal pending reduction and blocks another increase until battery feedback
reaches it. A zero previous command follows the normal zero write and
observed-neutral path. Immediate safety retreats remain higher priority, force
charge is unchanged, and demand beyond the active deadband at timeout still
uses the fault and cooldown behavior.

If acknowledgement takes 30 seconds, the regulator attempts zero and faults. A
timed-out magnitude increase also blocks only that command direction:

```text
first timeout since acknowledgement   -> 1 minute cooldown
later timeout without acknowledgement -> 10 minute cooldown
```

The cooldown timestamp remains nonzero after expiry. Continued demand may then
issue one normal bounded probe. If that magnitude increase is acknowledged, it
clears the timestamp. If it times out, it starts the 10 minute cooldown. An
acknowledged reduction does not prove the direction recovered and therefore
does not clear history. The opposite direction remains available and does not
clear the failed direction's history.

A reduction timeout keeps the existing zero, fault, and neutral-rearm behavior
without starting a cooldown. Slow unwind from a stronger command does not prove
that the direction is unavailable.

Cooldowns survive policy release and discrete mode handoff because neither
proves actuator recovery. They are in-memory only, so a process restart may
issue one new probe.

### 3. Immediate retreat

Raw grid error reduces battery effort when the current direction is wrong.

While charging:

```text
if error > activeDeadband:
    candidate = min(0, appliedCommand + error)
```

While discharging:

```text
if error < -activeDeadband:
    candidate = max(0, appliedCommand + error)
```

Rules:

- retreat is allowed while an increase is pending;
- each nonzero retreat replaces the pending acknowledgement;
- another increase remains blocked until feedback reaches the reduced command;
- further retreats remain allowed while a reduction is pending;
- retreat may exceed the 1500 W increase limit;
- retreat never crosses zero;
- a nonzero candidate below the 25 W write threshold snaps to exact zero;
- exact zero is always written.

Battery telemetry is still read and validated in the retreat cycle.

### 4. Bounded increase

When no command is pending:

```text
delta     = gain * error
delta     = clamp(delta, -1500 W, +1500 W)
candidate = appliedCommand + delta
candidate = clamp(candidate, directional limits)
```

Only a change of at least 25 W is written. The next increase is blocked until
the battery acknowledges this command.

There is no error filter. The 0.67 gain, 1500 W step limit, acknowledgement gate,
100 W startup deadband, and 50 W active deadband provide the damping. Removing
the filter also avoids extra lag when a cooktop or PV output changes quickly.

### 5. Direction reversal

No command may change sign directly.

To reverse:

1. retreat to exact zero;
2. cancel and stop the old actuator direction;
3. on a later cycle, read battery power;
4. require the sample to start after the zero write;
5. require absolute battery power at or below 300 W;
6. start the opposite direction with the normal bounded first step.

No additional dwell is required after neutral is observed.

### 6. Force charge

Force charge uses the same startup neutral barrier, step limit,
acknowledgement, and refresh behavior. It ramps toward the configured charge
limit independently of grid error. HEMS dimming or SoC policy may still disable
charging.

## Failure behavior

| Failure | Action |
|---|---|
| Grid read fails or becomes stale | Attempt zero immediately and fault |
| Battery feedback unavailable while the command is already zero | Fault without another write; require a later fresh near-zero sample |
| Battery feedback unavailable with valid grid, within 15 s | Hold or retreat; block all increases and refreshes |
| Battery feedback unavailable beyond 15 s | Attempt zero and fault |
| Policy expires | Attempt zero and fault |
| Power or SoC limits are unavailable | Release continuous control |
| Direction becomes disallowed | Stop to neutral |
| Magnitude increase is not acknowledged in 30 s | Attempt zero, fault, and block that direction for 1 minute or 10 minutes after a repeated failure |
| Reduction is not acknowledged in 30 s | Attempt zero and fault without blocking the direction |
| Nonzero write fails | Best-effort zero and fault |
| Zero write fails | Remain faulted; retry every healthy cycle for 1 minute, then once per minute |
| Mode handoff fails or its stop retry is backed off | Keep continuous policy released and do not write the discrete mode |
| More than one controller exists | Disable regulation and release all controllers |
| Loop overruns | Finish the current cycle; do not overlap |
| Shutdown | Close scheduler, release, join worker, then restore battery mode |

A write error is not treated like a harmless missed read. A multi-register
Huawei sequence may have succeeded partially, so the previous hardware command
cannot be assumed. Best-effort zero is the safe response. Stop retries remain
aggressive through Huawei's one-minute forced-control window. After that window,
bounded retries avoid monopolizing the SDongle connection during a persistent
write outage.

Fault recovery requires:

1. a successful zero;
2. a later fresh battery sample;
3. battery power within the 300 W neutral tolerance.

## Huawei actuator semantics

Continuous control uses:

- `47075`: configured/rated charge ceiling;
- `47077`: configured/rated discharge ceiling;
- `47083`: one-minute forced period;
- `47100`: stop, charge, or discharge direction;
- `47246`: forcible setting mode;
- `47247`: transient charge setpoint;
- `47249`: transient discharge setpoint.

The direction watchdog owns `47100` heartbeat writes. A stop must:

1. cancel the active direction watchdog;
2. write `47100=0`;
3. invoke the generic release sequence;
4. write `47100=0` again defensively;
5. restore `47075` and `47077` to configured/rated maxima.

Setup writes are skipped for a zero direction command so a failed ceiling,
setpoint, period, or strategy write cannot prevent the watchdog stop.

The adapter uses a full sequence for the first nonzero command, a direction
change, and every 30 second refresh. Between full sequences, same-direction
changes write only `47247` or `47249`. The periodic full sequence restores the
configured ceiling and strategy and renews the one-minute forced period. The
direction watchdog continues refreshing `47100` independently every 5 seconds.

The Huawei battery entry represents the aggregate energy storage system. It
reads power from `37765`, SoC from `37760`, total discharge from `37782`, and
total charge from `37780` through the shared `37760-37783` block read.
Continuous control uses the same aggregate feedback as the inverter-level
actuator and avoids the idle offset observed from the per-unit register
`37001`. Do not configure another battery entry for the same inverter because
site totals would count it twice.

## Initial tuning

| Parameter | Value |
|---|---:|
| Control interval | 5 s |
| Neutral startup deadband | 100 W |
| Active grid deadband | 50 W |
| Active discharge grid target | -20 W |
| Proportional gain | 0.67 |
| Maximum magnitude increase | 1500 W/cycle |
| Write threshold | 25 W |
| Acknowledgement tolerance | min(250 W, 50% of delta) |
| Acknowledgement movement | max(10 W, 25% of delta) |
| Maximum acknowledgement time | 30 s |
| First directional cooldown | 1 min |
| Repeated directional cooldown | 10 min |
| Neutral tolerance | 300 W |
| Policy maximum age | max(60 s, 2 x site interval) |
| Unchanged-command refresh | 30 s |

These are internal conservative starting values, not configuration API.

## Required tests

- startup writes zero and waits for observed neutral;
- unsaturated charging cases distinguish the `-residualPower / 2` target from
  nearby incorrect targets;
- unsaturated discharging cases distinguish the -20 W target from zero;
- negative residual does not start charging from grid import;
- neutral demand inside the 100 W startup deadband does not start control;
- active discharge accepts -70 W to 30 W grid power and corrects outside that
  range;
- active grid error above 50 W permits a correction of at least 25 W;
- delayed feedback cannot acknowledge a 25-50 W correction without movement;
- 25 W corrections can settle from at least 10 W directional movement;
- no second increase occurs before acknowledgement;
- a pending increase keeps its full acknowledgement window, then returns to the
  last acknowledged command if grid demand has disappeared, without changing
  cooldown history;
- rollback to a nonzero command waits for reduction acknowledgement, while
  rollback to zero requires observed neutral;
- delayed acknowledgement up to 20 seconds does not fault;
- missing acknowledgement at 30 seconds stops and faults;
- the first magnitude-increase timeout blocks only that direction for one
  minute;
- continued demand may issue one bounded probe when the cooldown expires;
- another timeout without acknowledgement blocks that direction for ten
  minutes;
- magnitude-increase acknowledgement clears only that direction's cooldown
  history;
- reduction acknowledgement does not clear cooldown history;
- the opposite direction remains available during cooldown;
- force charge respects the charging cooldown;
- release and mode handoff preserve cooldown history;
- retreat works while acknowledgement is pending;
- a nonzero retreat blocks re-increase until feedback reaches the reduced
  magnitude;
- chained retreats remain immediate and replace the pending acknowledgement;
- a reduction that does not settle within 30 seconds stops and faults without
  starting a cooldown;
- small retreat candidates snap to exact zero;
- retreat still reads fresh battery feedback;
- every reversal contains zero and a later neutral sample;
- invalid grid data stops immediately;
- battery feedback failure at an owned zero command does not rewrite zero;
- one transient battery feedback failure holds or retreats an existing command;
- feedback grace blocks increases, starts, reversals, and refresh writes;
- feedback grace expires 15 seconds after the last valid battery sample;
- grid failure still stops immediately while battery feedback is unavailable;
- successful slow Huawei reads are accepted and followed by a fresh grid read;
- stale policy stops;
- failed writes run best-effort zero;
- force charge follows the same step and acknowledgement limits;
- release and shutdown write zero;
- shutdown writes zero before waiting for a blocked meter read;
- a failed discrete mode transition cannot reactivate continuous control;
- one transient SoC read reuses a recent per-device value;
- an unknown adapter direction is stopped during acquisition;
- multiple configured controllers disable nonzero regulation;
- failed fallback release is retried by later site updates;
- scheduler cycles never overlap;
- periodic regulator reads are offset from the site polling phase;
- Huawei stop assertions verify that `47100=0` precedes restoration of
  `47075` and `47077`;
- failed Huawei release remains retryable;
- same-direction changes update only the active power register;
- first commands, reversals, and 30 second refreshes use the full Huawei
  sequence;
- unchanged commands refresh the forced period before expiry;
- Huawei battery measurements use the aggregate power, SoC, and energy
  registers through the shared `37760-37783` block read.

## Live validation

Before broad rollout, verify on Huawei hardware:

1. `SetBatteryPower(0)` stopping latency.
2. Battery power sign and first observable movement after a setpoint.
3. Behavior with 5, 10, 15, and 20 second response delay.
4. One unchanged command for more than two forced-control periods.
5. Ceiling restoration after charge, discharge, failed write, and shutdown.
6. Whether `47081` and `47082` remain enforced during forcible control.
7. Whether command churn and slow reads ever let the one-minute forced period
   expire before the 30 second full refresh; reduce the refresh interval if
   needed.
8. Stability while Huawei PV and battery meters share the SDongle through the
   configured Modbus proxy.
9. Aggregate register `37765` sign, magnitude, and zero-idle behavior during
   charge, discharge, and release.

The first enabled runs should remain supervised with conservative power limits
and a manual `47100=0` fallback available.
