# Reliable battery power regulation

## Status

Implementation design for a small, stateful battery regulator.

The initial target is one Huawei SUN2000 continuous battery controller using a
Shelly Pro 3EM grid meter and Huawei battery feedback through a Modbus proxy.
The generic `api.BatteryPowerController` actuator interface remains reusable,
but coordination of multiple continuous battery controllers is intentionally
out of scope for the first release.

## Decision summary

- Run one non-overlapping battery control loop every 3 seconds.
- Offset periodic regulator ticks by 1.5 seconds from the site loop phase.
- Keep the normal evcc site interval at its 30 second default.
- Support exactly one active continuous battery power controller per site.
- Read fresh grid and battery power during every normal control cycle.
- Use the raw directional grid error without an error filter or PID state.
- Keep a 50 W deadband both for starting from neutral and while a direction is
  active.
- Bound normal command magnitude increases to 1500 W per cycle.
- For discharge increases only, use gain 1.0 and a 2000 W step limit on the
  first measured grid import above 500 W. Raise the step limit to 4000 W after
  a second consecutive sample above 500 W.
- Require battery acknowledgement before another magnitude increase.
- If an established discharge collapses to near-zero feedback for two
  consecutive samples while the grid is still importing, treat that as a
  lost actuator: do not increase, then zero and apply the discharge
  cooldown. A single sample must not trip.
- After reducing a nonzero command, wait for battery feedback to reach the
  reduced magnitude before increasing again, unless two consecutive samples
  show more than 500 W import while discharging. Further reductions remain
  immediate.
- Reduce an unsafe command immediately. A reduction may reach zero in one
  cycle, but it must never cross zero.
- Always write exact zero. Never suppress zero because of the write threshold.
- Acquire ownership, reverse direction, and recover from faults only through a
  successful zero command followed by a later near-zero battery sample.
- Preserve one quarter of the configured site residual power while charging
  from PV.
- Center active discharge control at 50 W grid export so its 50 W deadband
  accepts grid power from 100 W export to zero grid power.
- Stop on invalid grid data, battery feedback unavailable for 15
  seconds, stale policy, write failure, failed mode handoff, release, or
  shutdown.
- During the battery feedback grace period, keep the active command or reduce
  it from fresh grid data. Never increase, start, reverse, or refresh it.
- Avoid same-cycle retries except for the best-effort zero after a failed write.

The regulator deliberately adds battery effort carefully and removes battery
effort quickly. The faster discharge correction handles sustained large import
transients without bypassing a pending increase. Confirmed import may replace a
pending discharge reduction so a preceding safety retreat does not block the
response to a new load. It does not activate for export or charging.

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
    LastFastImportAt      time.Time
    DischargeFollowed     bool
    LostDischargeStreak   int
    LastWriteAt           time.Time
    Policy                ControlPolicy
    ChargeBlockedUntil    time.Time
    DischargeBlockedUntil time.Time
}
```

There is no filtered error, accumulated integral, or multi-battery allocation
state.

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

The 3 second regulator does not read SoC.

Continuous control requires both power limits and SoC limits on the controlled
battery. Missing power or SoC limits keep the policy inactive.

The site keeps the last successful per-device SoC for up to 60 seconds. That
cache is independent of policy maximum age. One transient SoC read failure
therefore does not stop and restart regulation, while prolonged SoC loss still
disables both directions.

SoC eligibility uses the exact configured limits. Charging remains allowed while
SoC is below the configured maximum, and discharging remains allowed while SoC
is above the configured minimum. The regulator does not add headroom that could
prevent a battery from reaching its configured maximum.

Fast and planned EV charging use `batteryDischargeControl` and `bufferSoc`:

- prevent on: hold discharge immediately;
- prevent off and a usable buffer (`0 < bufferSoc < 100`): allow support until
  SoC reaches `bufferSoc`, then hold until that fast or planned StatusC session
  ends;
- prevent off and buffer 0 or 100: no hold.

The buffer hold is latched so a small SoC rebound does not repeatedly release
and reapply discharge control. Changing `bufferSoc` clears the latch. If SoC is
stale while a usable buffer is set, discharge stays held for that evaluation.

### Battery regulator

The regulator:

- owns all continuous signed power commands;
- reads the site's existing grid and controlled battery meter instances;
- forwards independently valid grid and controlled-battery power samples to the
  site's live meter state;
- applies deadband, gain, and step limiting;
- gates increases on actuator acknowledgement;
- enforces startup and reversal neutrality;
- exposes a lock-safe effective charging reservation to the site loop;
- releases ownership before discrete mode control.

Below `prioritySoc`, the site loop adds this reservation only to the power signal
used by PV loadpoints. Measured and published grid, battery, home, and green-share
values remain unchanged, as does the regulator's grid target. The reservation is
available only with fresh SoC and charge eligibility, valid limits, permitted
charging, an available continuous controller, and no charging cooldown or fault.
The site rebuilds charge eligibility from the meter snapshot before each
loadpoint decision, so startup does not depend on the full regulator policy being
stored at the end of that decision. A healthy controller that is temporarily
discharging may retain the reservation while it unwinds through neutral toward
charging; stale battery feedback or a released controller still removes it. The
reservation initially uses the configured charge limit so a neutral battery can
claim surplus before measured charging appears. While a command is pending or
feedback has caught up, that limit remains the effective claim. Once anti-windup
holds a settled command whose feedback still trails, the claim falls to observed
charging power so surplus beyond the proven battery capability remains available
to loadpoints. The configured limit is already an AC limit. Only a reservation
derived from observed battery power subtracts excess DC, because that measurement
includes power that cannot be redirected to an AC loadpoint.

Batteries without continuous power control retain the existing measured-power
priority behavior. Battery boost loadpoints receive the inverse reservation
adjustment and therefore continue to see the unadjusted surplus.

Loadpoint current, enable, and phase-switch commands that can leave AC demand
above fresh measured power add or replace a separate short-lived feed-forward
claim before the charger write. The
claim is the expected AC target minus fresh measured loadpoint power, after
circuit and current limits. A
continuously charging battery immediately reduces its command by that amount and
cannot increase charging again while demand remains unacknowledged. Fresh
physical charge power or phase-current measurements reduce the claim as the load
appears. Synthetic charge-meter values and failed reads cannot acknowledge it.
Only growth in aggregate outstanding demand causes another retreat, applied as
an incremental reduction from the battery's current command. Acknowledgement or
claim removal never increases battery charging, but a later claim increase still
retreats by its positive delta.
Without fresh physical demand, whether the meter is absent or its read failed,
an initial enable claims the full target. Later current and phase changes adjust
the existing unacknowledged claim by the signed command delta, so a 6 A to 7 A
increase adds only 1 A unless the earlier 6 A claim is still pending. A phase
change uses the previous and requested effective phase counts at the offered
current. A following current change replaces that phase claim with the combined
target instead of double-counting it. A lower command retains any demand that is
still above fresh physical or command-estimated load.

The claim is rolled back if the first actuator write fails. A successful phase
switch commits its claim before any following current write. If a current write
succeeds but a following enable write fails, the claim is retained because the
first write may already have changed physical demand. Continuous-policy release
also retains unexpired demand, so reacquisition cannot increase battery charging
until the load is acknowledged or the claim expires. Disable or a physically met
target removes the claim. Every claim expires after 45 seconds if the physical
load never appears. It does not modify the regulator grid target or any measured
or published power.

The first failed battery feed-forward write rejects the triggering loadpoint
command and enters normal fault stopping. Further claims while control is
fault-stopping or released are recorded without battery writes and do not block
loadpoint actuation. Fault recovery and reacquisition remain the only paths that
may write the battery, and an unexpired recorded claim still gates later charging
increases.

Site coordination currently holds `coordinationMu` across that battery write.
That keeps the claim map and the regulator demand atomic, so another loadpoint
cannot raise current before the retreat is accepted. The 3 second tick does not
take this lock. A later improvement is to snapshot a generation, unlock for the
write, and republish if the map changed, so pending-demand reads do not wait on
Huawei I/O.

The same remaining claim is used by loadpoint priority handover. This prevents a
pending higher-priority start from being counted twice and releases lower
loadpoints progressively as physical demand appears.

The live-meter callback runs asynchronously and never changes the regulator's
control result. `Site` merges samples by completion timestamp into a separately
locked copy of the published grid and battery state. It preserves structured
meter details, updates the controlled battery by site meter index, recomputes the
multi-battery total, and derives `homePower` from the latest published grid,
battery, PV, and loadpoint charge values. The normal site-loop fields remain the
30 second control snapshot.

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

Huawei exposes both interfaces. Any modified mode, including grid Charge,
therefore uses this discrete handoff. The regulator force-charge ramp below
does not run on Huawei.

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
chargeGridTarget     = -residualPower / 4
dischargeGridTarget  = -50 W
error                = gridPower - activeGridTarget
```

Charging therefore retains one quarter of the residual export margin used for
PV charging at a loadpoint. The faster battery regulator can harvest more of a
small sustained surplus while retaining margin for meter noise and inverter
response. Discharging uses a biased grid deadband from -100 W to 0 W. This
favors export and corrects any sustained import once discharge is active.

From neutral:

```text
chargeStartTarget    = min(chargeGridTarget, 0 W)
dischargeStartTarget = max(dischargeGridTarget, 0 W)

gridPower < chargeStartTarget - startDeadband    -> request charge
gridPower > dischargeStartTarget + startDeadband -> request discharge
otherwise                                       -> remain neutral
```

A negative residual power may allow one quarter of the configured import during
established PV charging, but it must not start charging from neutral solely
because the site is importing. The 50 W startup deadband prevents cycling
between neutral and a small command while capturing smaller sustained PV
surpluses and correcting smaller imports. With the normal gain, crossing the
deadband produces at least a 34 W command, which remains above the 25 W write
threshold. Direction reversals still require a successful zero command and a
later near-zero battery sample, so reducing the startup deadband does not weaken
the reversal barrier.

The discharge start target remains clamped to zero, so the battery does not wake
only to create export. It starts above 50 W import and then regulates toward
50 W export. With a 200 W residual power, charging starts below 100 W export and
then regulates toward 50 W export. These thresholds reduce unharvested solar
surplus and tolerated grid import while retaining 100 W of hysteresis across
each active band.

At the nominal -50 W discharge center, continuous export is 0.4 kWh over 8 hours
or 0.6 kWh over 12 hours. The accepted discharge band can export up to 100 W.
This is an intentional self-consumption tradeoff for avoiding steady grid
import. Transient imports can still occur because of the three-second control
cadence and inverter response. Sites that prohibit grid export must not use this
tuning.

## Scheduler and sample validity

- One regulator goroutine per site.
- One synchronous `tick` at a time.
- No queued catch-up cycles.
- Run one immediate tick, then start the three-second cadence at a 1.5-second
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
- the completed grid sample is older than one 3 second control interval when
  the command decision is made.

The meter API cannot cancel an in-flight read. A cycle that blocks cannot
overlap another cycle. Huawei's watchdog and one-minute forced period remain the
out-of-band safety limits if an I/O call does not return.

A successful Huawei read is accepted regardless of its duration. Its completion
timestamp represents the fresh feedback point, and the Shelly grid read happens
afterward. Rejecting successful 4.9-9.1 second responses would only force a
stop after current feedback had already arrived.

Every cycle with valid grid and battery samples writes one TRACE snapshot
before acknowledgement or control decisions. The regulator uses the dedicated
`battery` log area so cycle traces can be enabled without tracing all of `site`:
`--log info,site:debug,battery:trace`. The stable `cycle=<n>` record includes the
phase, fresh grid and battery power, applied and pending commands, active target
and raw error, selected demand direction, direction availability, force-charge
and neutral state, command ages, last command action, stop retry timing, and
sample read timing. Action, fault, stop, acquire, release, timeout, and cooldown
logs stay on DEBUG or ERROR. A raw-grid safety retreat includes the same cycle
ID in its existing command action log so delayed Huawei feedback, the Shelly
sample, and the resulting retreat can be correlated without another action
record.

Grid and battery samples are validated independently for publication. Invalid,
stale, non-finite, and out-of-order samples are not published. Releasing the
regulator invalidates pending callback work, including a cycle whose meter read
finishes after release. Published grid and battery structs are deep copies so
later site updates cannot mutate values already handed to WebSocket, REST, MQTT,
or InfluxDB consumers.

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
- a grid failure, policy invalidation, active-direction becoming disallowed,
  write failure, release, or shutdown still commands zero immediately;
- an active-direction power limit drop clamps or retreats without a zero
  handoff;
- expiry commands zero and enters normal fault recovery.

Fifteen seconds permits one recovery attempt after a failed read that begins
three seconds after the previous sample and itself takes about five seconds. A
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
battery sample acknowledges the command. The only exception is that two
consecutive fresh samples above 500 W import may replace a pending discharge
reduction. A pending increase is never superseded.

Pending acknowledgement is armed only when a command materially differs from
the latest measured battery baseline:

```text
abs(command - baselinePower) >= max(250 W, 10% of abs(command))
```

Huawei battery telemetry carries enough noise that smaller changes cannot be
reliably proven through measurement, so an immaterial command still writes
normally but skips the pending/timeout/cooldown machinery. A run of ignored
immaterial changes still accumulates against the unchanged baseline, so an
escalating command eventually turns material and arms pending acknowledgement;
unlimited unacknowledged escalation is not possible. This materiality gate is
distinct from the acknowledgement tolerances below and from the 300 W neutral
tolerance; it only decides whether a command needs to arm pending
acknowledgement at all, and it does not apply to the timeout rollback of an
undemanded increase, which always arms so the rollback itself is proven unless
confirmed sustained import demands discharge again.

For a magnitude increase, acknowledgement succeeds when either:

- measured battery power reaches the command direction within
  `min(250 W, 50% of command delta)`; or
- battery power moved toward the command by at least
  `max(10 W, 25% of command delta)`.

For a reduction, acknowledgement succeeds when either feedback reaches the
safer side of the new command within
`min(250 W, 50% of command delta)`:

```text
charging:    measuredPower >= command - adaptiveTolerance
discharging: measuredPower <= command + adaptiveTolerance
```

Or feedback must have moved strictly to the safer side of `PreviousCommand` and
the remaining command-to-feedback gap must be immaterial under the same
materiality rule that armed acknowledgement. This second path handles chained
retreats whose measurement baseline is far from the latest small reduction.
Feedback equal to or stronger than `PreviousCommand` cannot acknowledge it.

Partial movement alone does not acknowledge a reduction because it would permit
the next increase while Huawei may still be applying a materially stronger
command. Unchanged feedback cannot pass the relaxed path: the pending command
was armed because its gap from that same baseline was material.

Grid movement cannot acknowledge a battery command because PV and household
load may change independently.

If the 30 second acknowledgement window expires after grid demand for a pending
magnitude increase has returned to the active deadband, the regulator abandons
the increase and writes `PreviousCommand`, the last acknowledged command. It
does not roll back earlier because grid and battery feedback may arrive with
different delays. This rollback is not an acknowledgement and does not create
or clear directional cooldown history. A nonzero previous command becomes a
normal pending reduction and blocks another increase until battery feedback
reaches it, unless confirmed sustained import demands discharge again. A zero
previous command follows the normal zero write and observed-neutral path.
Immediate safety retreats remain higher priority, force charge is unchanged,
and demand beyond the active deadband at timeout still uses the fault and
cooldown behavior.

If acknowledgement takes 30 seconds, the regulator attempts zero and faults,
unless the timed-out command is a charging magnitude increase and the
charging saturation hold below applies instead. A timed-out magnitude
increase that does fault also blocks only that command direction:

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

#### Charging saturation hold

A timed-out charging magnitude increase is a safe saturation hold, not a
fault, when charging was already materially established before the increase
and the current battery feedback is not materially in the wrong direction.
This is feedback-based, not SoC-based: it reacts to what the battery
actually reports, not to a hard-coded state of charge, because BMS taper
onset varies by device and conditions. It never applies to discharge; a
timed-out discharge increase always keeps the existing fault, cooldown, and
rearm behavior.

Established charging requires both a nonzero prior charging command and a
materially nonzero charging baseline at the time the increase was applied,
reusing the same materiality gate as the acknowledgement gate above (with
`command = 0`, i.e. `abs(baselinePower) >= max(250 W, 10% of abs(command))`).
An immaterial, unproven previous command or a baseline near neutral does not
count as established, so a first command from neutral still hard-stops.

Given established charging, the hold applies when the final battery reading
still materially trails the applied command in the charging direction:

```text
batteryPower > command             // not yet caught up (charging command is negative)
batteryPower <= neutralTolerance    // not materially wrong-direction (300 W)
abs(command - batteryPower) >= max(250 W, 10% of abs(command))
```

A small positive reading up to the existing 300 W neutral tolerance is
treated as Huawei taper or noise and is still eligible for a hold; a larger
positive reading is material discharging while charging was commanded and
always hard-stops instead.

On a hold, the regulator keeps the applied command, clears the pending
acknowledgement, remains in the `charging` phase, does not write zero, does
not enter `neutral`/`faultStopping`, and does not start or clear directional
cooldown history. It logs one concise diagnostic with the command, previous
command, baseline, final battery power, grid power, SoC, and elapsed time.
No dedicated hold state is recorded: there is no flag, timer, learned
ceiling, or extra cooldown timestamp for the hold itself.

#### Stateless charging anti-windup gate

Before *every* charging magnitude increase, not only one following a hold,
the regulator refuses to escalate unless measured battery feedback has
genuinely caught up with the currently applied charging command. This
applies identically to normal control and to force charge:

```text
batteryPower <= command   // caught up or exceeded: release the increase
```

Otherwise, when the battery still trails the applied command, the increase
is refused for that cycle: no write, no new pending command. The gate is
unconditional and stateless. It runs on every charging increase attempt
using only the currently applied command and the latest valid battery
reading; it holds no memory of whether a saturation hold ever occurred. It
is naturally inert for a fresh start from neutral, where the applied command
is zero.

This single gate is what prevents runaway escalation past a saturated
actuator: once a hold keeps the applied command in place while feedback
still trails it, the same gate keeps refusing further increases on later
cycles for as long as the gap persists, until measured feedback catches up
or charging is disabled/reset (zero command, released, mode handoff). Under
normal grid-following control, an immediate safety retreat can also release
the gap by reducing the applied command outright if grid conditions force a
reduction; it is unaffected by this gate and continues to run before the
pending gate every cycle, so grid import is still reduced immediately
regardless of any trailing feedback, and does so even before any hold or
timeout would otherwise occur. Force charge does not use immediate retreat
(it is intentionally disabled while `forceCharge` is set), so an
established force-charge ramp only recovers from a trailing gap by feedback
catching up, or by charging being disabled/reset.

#### Wrong-direction feedback always hard-stops

A materially wrong-direction reading (battery discharging beyond the 300 W
neutral tolerance under an applied charging command) never counts as caught
up, so the anti-windup gate above never releases on it; only genuine
catch-up may permit a further increase. But a gate that only refuses to
*increase* is not sufficient on its own: with no pending command in flight
(for example right after a saturation hold clears it, or after a normal
acknowledged command), silently refusing forever would leave a charging
command applied indefinitely while the battery is measurably discharging.
So, when no command is pending, every cycle also checks the currently
applied command against the freshly read battery sample: whenever charging
is applied and that reading is materially wrong-direction, the regulator
immediately writes zero, enters `faultStopping`, and arms the same
first/repeated cooldown as a failed charging magnitude increase, so rearm
cannot immediately repeat it. This check runs before the force-charge and
normal-control branches, so it covers both. It does not run while a
command is pending, because `tick()` returns from the pending timeout and
rollback block first.

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
- another increase remains blocked until feedback reaches the reduced command,
  unless two consecutive samples show more than 500 W import while discharging;
- further retreats remain allowed while a reduction is pending;
- retreat may exceed the selected increase limit;
- retreat never crosses zero;
- a nonzero candidate below the 25 W write threshold snaps to exact zero;
- exact zero is always written.

Battery telemetry is still read and validated in the retreat cycle.

### 4. Bounded increase

When no command is pending, or confirmed import supersedes a pending discharge
reduction:

```text
gain, maximum step = 0.67, 1500 W
if increasing discharge and measured grid import > 500 W:
    gain, maximum step = 1.0, 2000 W
    if the preceding sample also exceeded 500 W:
        maximum step = 4000 W

delta     = gain * error
delta     = clamp(delta, -maximum step, +maximum step)
candidate = appliedCommand + delta
candidate = clamp(candidate, directional limits)
```

Only a change of at least 25 W is written. The next increase is blocked until
the battery acknowledges this command. Two consecutive import samples may
replace a pending discharge reduction, but never a pending increase. The first
large-import sample retains the 2000 W limit. The preceding sample must be no
more than 4.5 seconds old, preventing one-sample load spikes and slow read gaps
from selecting the 4000 W step.

There is no error filter. The normal 0.67 gain and 1500 W step limit, the
discharge-only transient parameters, acknowledgement gate, 50 W startup
deadband, and 50 W active deadband provide the damping. Removing the filter also
avoids extra lag when a cooktop or PV output changes quickly.

### 5. Direction reversal

No command may change sign directly.

To reverse:

1. retreat to exact zero;
2. cancel and stop the old actuator direction;
3. on a later cycle, read battery power;
4. require the sample to start after the zero write;
5. require absolute battery power at or below 300 W;
6. start the opposite direction with the bounded step selected from fresh grid
   samples. A 4000 W discharge step requires two consecutive samples above
   500 W import, including samples observed while waiting for neutrality.

No additional dwell is required after neutral is observed.

### 6. Force charge

The regulator force-charge ramp is only for a power-only meter: one that
implements `api.BatteryPowerController` and not `api.BatteryController`.
On such a meter, site policy stays active in Charge mode and the regulator
ramps charge. On Huawei, policy is inactive whenever a discrete mode is
requested, so this ramp never runs.

Force charge uses the same startup neutral barrier, step limit,
acknowledgement, and refresh behavior. It ramps toward the configured charge
limit independently of grid error. HEMS dimming or SoC policy may still disable
charging.

A meter that also implements `api.BatteryController`, including the Huawei
template, must not use this ramp. Grid charge on that hardware is discrete
`BatteryCharge` after continuous control has been released. Do not drive
Huawei Charge through signed watt setpoints without new field evidence.

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
| Magnitude increase is not acknowledged in 30 s | Attempt zero, fault, and block that direction for 1 minute or 10 minutes after a repeated failure, unless the charging saturation hold below applies |
| Charging magnitude increase is not acknowledged in 30 s, charging already established, feedback not materially wrong-direction | Charging saturation hold: keep the applied command, clear pending, remain charging; the stateless anti-windup gate below then blocks further increases until feedback catches up, or (under normal grid-following control only) a safety retreat fires |
| Applied charging command's freshly read feedback is materially wrong-direction, no pending command in flight (e.g. after a hold or an acknowledged command) | Attempt zero, fault, and block charging for 1 minute or 10 minutes after a repeated failure, same as a failed magnitude increase |
| Established discharge feedback stays near zero for two consecutive samples while the grid is importing | Do not increase. Then attempt zero, fault, and block discharging for 1 minute or 10 minutes after a repeated failure. A single sample does not trip, and a probe that has never been followed is not established |
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
| Control interval | 3 s |
| Neutral startup deadband | 50 W |
| Active grid deadband | 50 W |
| Active discharge grid target | -50 W |
| Normal proportional gain | 0.67 |
| Normal maximum magnitude increase | 1500 W/cycle |
| Fast discharge import threshold | >500 W |
| Fast discharge proportional gain | 1.0 |
| First fast discharge maximum magnitude increase | 2000 W/cycle |
| Confirmed fast discharge maximum magnitude increase | 4000 W/cycle |
| Write threshold | 25 W |
| Acknowledgement tolerance | min(250 W, 50% of delta) |
| Acknowledgement movement | max(10 W, 25% of delta) |
| Maximum acknowledgement time | 30 s |
| First directional cooldown | 1 min |
| Repeated directional cooldown | 10 min |
| Neutral tolerance | 300 W |
| Policy maximum age | max(90 s, 3 x site interval) |
| Unchanged-command refresh | 30 s |

These are internal conservative starting values, not configuration API.

## Field observations and tuning rationale

The initial Huawei deployment was tuned from controller debug logs rather than
from a synthetic actuator model. Energy figures below integrate the grid-power
sample from each control cycle over its 3 second interval. They are gross import
and export seen by the controller, not utility billing-register deltas. Results
from different days are directional because household loads, PV production, SoC,
and charge availability differ.

### Controller evolution

The original controller used a 5 second interval, gain 0.67, a 1500 W increase
limit, and no special handling for large import. A complete baseline day showed:

- 0.588 kWh gross grid import after excluding an unrelated network outage;
- 0.426 kWh while the battery was discharging;
- 0.316 kWh of import above 500 W;
- 0.266 kWh while command acknowledgement was pending;
- only 0.019 kWh of discharge import at or below 30 W.

This attribution showed that steady-state target error was not the main loss.
Large load transitions and delayed Huawei setpoint response dominated. Moving the
discharge target farther into export could therefore save little while creating
continuous export.

The controller was then changed to the current 3 second interval and guarded
fast-discharge path. The first import sample above 500 W permits a 2000 W
increase. A second fresh sample within 4.5 seconds permits 4000 W. Confirmed
import may also replace a pending discharge reduction, but a pending increase
still requires actuator acknowledgement.

A 6 hour observation with these changes and a 2 second Modbus proxy cache
showed 0.212 kWh total import and 0.156 kWh discharge import. Over the comparable
clock window, normalized discharge import fell from 128 Wh/h with the initial
2000 W fast path to 81 Wh/h. Relative to the original controller over the same
6 hour clock window, discharge import fell from 0.323 kWh to 0.156 kWh. These
comparisons are promising but are not controlled load tests.

### Full-day observation

A subsequent log covered exactly 24 hours from 2026-08-12 20:44 to
2026-08-13 20:44. It contained 28,800 control samples and measured:

| Scope | Duration | Gross import | Gross export |
|---|---:|---:|---:|
| Complete log | 24 h | 0.300 kWh | 30.532 kWh |
| Calendar day to 20:44 | 20.74 h | 0.265 kWh | 30.424 kWh |

Calendar-day import was distributed as follows:

| Regulator phase | Import |
|---|---:|
| Discharging | 0.148 kWh |
| Charging | 0.077 kWh |
| Neutral | 0.039 kWh |
| Fault | <0.001 kWh |

Discharge import between 300 W and 500 W was only 0.009 kWh. Import while a
discharge increase was pending was 0.043 kWh, and import while a discharge
reduction was pending was 0.029 kWh. Pending-state energy is not itself fully
avoidable because Huawei feedback commonly needs 9 to 15 seconds to reflect a
new setpoint.

For import events above 30 W, the first positive sample contributed 0.103 kWh.
The second and third samples contributed another 0.090 kWh. Only 0.057 kWh
remained after the first three samples. Reactive control cannot anticipate the
first sample, and much of the next two samples falls inside the measured Huawei
response time. A realistic further saving from controller tuning is therefore
about 0.02 to 0.04 kWh for a similar day, or 8% to 15% of the observed
calendar-day import. An aggressive upper estimate is 0.05 to 0.07 kWh, with greater
command churn, battery cycling, and export overshoot risk.

One event at 06:48 was not normal control latency. Huawei ignored an 820 W
discharge command for 33 seconds at 21% SoC. The acknowledgement timeout stopped
control and blocked discharge for one minute, and the complete event contributed
about 0.015 kWh import. Treat repeated occurrences as an inverter, SoC-limit, or
communication investigation. Do not shorten the safety cooldown merely to hide
an ignored command.

### Why the current values remain

- Keep the -50 W discharge target. Its 50 W deadband accepts 100 W export
  to zero grid power, so there is no steady discharge-import band. A more
  negative target would create export continuously.
- Keep the 500 W fast threshold. The measured 300 W to 500 W discharge band
  contributed only 0.009 kWh/day, too little to justify more frequent fast
  commands.
- Keep the first fast step at 2000 W. A 14.5 kW import spike lasted one sample,
  and several other large loads disappeared before Huawei could react. A larger
  first step would mainly increase subsequent export.
- Keep the confirmed fast step at 4000 W. It activated selectively for sustained
  multi-kilowatt import and showed no runaway escalation.
- Keep acknowledgement gating for pending increases. Huawei often applies
  commands after several cycles; repeated full corrections during that delay
  would wind up the command and overshoot when feedback arrives.
- Keep immediate reductions and the pending-reduction override. New load may
  legitimately arrive while an export-driven retreat is still pending.
- Keep the 3 second interval. A 2 second interval offers only an estimated 0.01
  to 0.02 kWh/day improvement while increasing Modbus traffic and still cannot
  remove inverter response latency.
- Keep the 50 W neutral startup deadband. It matches the active deadband
  and still avoids starting the battery for low-power, potentially
  short-lived demand.
- Keep the timeout and cooldown behavior. It exposed a real ignored command and
  prevented repeated blind escalation.

The Modbus proxy cache should remain shorter than the control interval. Reducing
its TTL from 4 seconds to 2 seconds changed median battery-read latency from
about 1.6 ms, characteristic of frequent cache hits, to about 9.5 ms with a
12.9 ms p90, characteristic of mostly fresh upstream reads. Request delay,
connection delay, timeouts, and stale-serving behavior did not require changes.

### Possible future experiments

Collect several complete days and compare controller-integrated import with the
utility meter before changing behavior. Revisit tuning only if the same
attribution repeats across different load and solar conditions.

The next bounded experiment would be pending-increase promotion:

- require an existing pending discharge increase;
- require grid import above 2 kW for two additional fresh samples;
- permit only one promotion before acknowledgement;
- add no more than 2 kW;
- cap the total unacknowledged increase at about 6 kW;
- preserve immediate export retreat, zero handoff, and reversal safeguards.

Expected benefit is about 0.005 to 0.015 kWh on a similar day. Do not combine this
experiment with a lower fast threshold or shorter interval, because its export
cost must be measured independently.

Lowering the 500 W fast threshold should only be reconsidered if repeated
logs show meaningful energy in the 300 W to 500 W band without corresponding
short load oscillations.

## Required tests

- startup writes zero and waits for observed neutral;
- unsaturated charging cases distinguish the `-residualPower / 4` target from
  nearby incorrect targets;
- unsaturated discharging cases distinguish the -50 W target from zero;
- negative residual does not start charging from grid import;
- neutral demand inside the 50 W startup deadband does not start control;
- active discharge accepts -100 W to 0 W grid power and corrects outside that
  range;
- active grid error above 50 W permits a correction of at least 25 W;
- discharge import below or at 500 W uses the normal gain and step limit;
- the first discharge import sample above 500 W uses gain 1.0 and remains
  capped at 2000 W;
- a second consecutive discharge import sample above 500 W raises the step cap
  to 4000 W;
- confirmed import may replace a pending discharge reduction but not a pending
  increase;
- charging always retains gain 0.67 and the 1500 W step limit;
- normal and fast corrections remain clamped to configured power limits;
- delayed feedback cannot acknowledge a 25-50 W correction without movement;
- 25 W corrections can settle from at least 10 W directional movement;
- no second increase occurs before acknowledgement;
- tiny commands that do not materially differ from the battery baseline do not
  arm pending acknowledgement or later time out;
- a small correction close to a high-output baseline stays immaterial and does
  not arm pending acknowledgement;
- ignored immaterial changes accumulate against the unchanged baseline until
  the gap becomes material and arms pending acknowledgement;
- a low-power reduction close to baseline does not create a false pending
  timeout;
- a genuine material increase still arms pending acknowledgement and follows
  the existing timeout, stop, cooldown, and rearm path;
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
- a charging magnitude-increase predicate table proves exact material
  threshold and caught-up feedback for applied-command-vs-measured-power
  trailing, including the boundary at the material floor and a small
  positive taper reading within the neutral tolerance;
- the stateless anti-windup gate blocks a charging increase immediately,
  before any pending command or 30 s timeout exists, whenever measured
  feedback materially trails the currently applied charging command,
  reproducing the live 16:21:18 shape (applied -5298 W, measured -4633 W,
  demand otherwise -5818 W);
- each of the three live charging timeout shapes (taper and collapse under
  persistent export) reproduces as a saturation hold: no zero write, no
  fault-stopping, no cooldown, pending cleared, phase remains charging,
  applied command held, and a following cycle does not escalate while
  feedback still materially trails;
- an immediate safety retreat still fires while feedback trails the applied
  command, in case grid import appears after a hold;
- feedback catching up to the applied command permits a later increase,
  both immediately (before any hold) and after a saturation hold;
- a first charging command from neutral with no measured response still
  hard-stops, cools down, and rearms, because charging was not yet
  established;
- established charging with materially wrong-direction feedback (material
  discharging beyond the neutral tolerance) still hard-stops instead of
  holding, both when a pending increase times out and, once no command is
  pending, on any later cycle where feedback turns wrong-direction
  (for example right after a saturation hold clears pending), with the same
  cooldown as a failed magnitude increase;
- a timed-out discharge magnitude increase is completely unaffected by the
  charging saturation hold and keeps the existing fault, cooldown, and
  rearm behavior;
- magnitude-increase acknowledgement clears only that direction's cooldown
  history;
- reduction acknowledgement does not clear cooldown history;
- the opposite direction remains available during cooldown;
- force charge respects the charging cooldown;
- an established force-charge increase that times out with feedback still
  trailing becomes a saturation hold like normal control, and the same
  stateless anti-windup gate then blocks further 1500 W ramp steps until
  feedback catches up;
- release and mode handoff preserve cooldown history;
- retreat works while acknowledgement is pending;
- a nonzero retreat blocks re-increase until feedback reaches the reduced
  magnitude or crosses the previous command with only an immaterial residual
  gap, unless confirmed sustained import replaces the reduction;
- the three live discharge-reduction timeout shapes acknowledge through that
  settled-response path, while unchanged, stronger, and materially distant
  feedback remain pending;
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
