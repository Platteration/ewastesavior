// Package power is the node's battery, thermal, lid and memory policy
// (DESIGN 10.4) as pure logic: a metrics sample and the previous decision
// in, the next decision out. It has no I/O, so the node agent calls
// Evaluate once per sample and tests can replay whole sequences.
package power

import (
	"fmt"
	"math"
	"time"

	"github.com/platteration/ewastesavior/internal/proto"
)

// Policy constants.
const (
	// DefaultMaxTempC is the pause threshold when neither the sensor-derived
	// limit nor Policy.MaxTempC is known (max_temp_c's default).
	DefaultMaxTempC = 85.0
	// ResumeBelowC is the thermal hysteresis: a heat pause ends at
	// limit - ResumeBelowC.
	ResumeBelowC = 10.0
	// ThrottleSamples is how many consecutive samples with rising
	// ThrottleEvents cause a pause.
	ThrottleSamples = 3
	// MemPressureLimit is the PSI memory "some avg10" (percent) at which new
	// work is refused.
	MemPressureLimit = 30.0
	// MinBatteryHealthPercent: a battery worn below this is treated as absent
	// for preemption, because its charge reading is meaningless.
	MinBatteryHealthPercent = 20
	// BatteryResumeMargin is the battery hysteresis: after a low-battery
	// preemption, work is accepted on battery again only at
	// BatteryMinPercent + BatteryResumeMargin (or on AC).
	BatteryResumeMargin = 5
)

// Policy is the operator's power configuration.
type Policy struct {
	RunOnBattery      bool    // run_on_battery: keep accepting work on battery
	BatteryMinPercent int     // battery_min_percent: preempt running tasks below this on battery
	MaxTempC          float64 // max_temp_c: used when Metrics.CPUTempLimitC is unknown
}

// Input is one evaluation's view of the machine.
type Input struct {
	Metrics proto.Metrics
	// ExternalDisplay reports a connected non-internal connector
	// (hwinfo.ExternalDisplayConnected).
	ExternalDisplay bool
	// MemBudgetMB is the node's memory budget (DESIGN 10.3). <= 0 disables
	// the swap rule.
	MemBudgetMB int
}

// Decision is the policy outcome. The zero value is a valid initial prev.
type Decision struct {
	Accept       bool // take new tasks; when false the node's free capacity is 0
	Pause        bool // freeze running tasks (thermal); implies !Accept
	Preempt      bool // hand running tasks back (battery low); implies !Accept
	BlankDisplay bool // lid closed with no external display; compute is unaffected
	// Reason explains why work is refused or paused, first cause first:
	// battery preemption, heat, on battery, memory. "" when Accept.
	Reason string
	// Since is the Metrics.Time at which Accept, Pause or Preempt last
	// changed.
	Since time.Time

	// Hysteresis state carried from sample to sample.
	thermal        bool   // paused by heat or throttling until cooled
	lowBattery     bool   // preempted for low battery until recovered
	haveThrottle   bool   // lastThrottle holds a sample
	lastThrottle   uint64 // previous Metrics.ThrottleEvents
	throttleStreak int    // consecutive samples in which ThrottleEvents rose
}

// Evaluate applies DESIGN 10.4 to one metrics sample:
//
//   - Heat: limit = Metrics.CPUTempLimitC, else p.MaxTempC, else
//     DefaultMaxTempC. Pause when CPUTempC >= limit, or when ThrottleEvents
//     rose in ThrottleSamples consecutive samples. A pause lasts until the
//     temperature is <= limit - ResumeBelowC (or unknown) and no new throttle
//     events occurred in that sample.
//   - Battery: on battery and not p.RunOnBattery, Accept = false. On battery
//     below p.BatteryMinPercent, Preempt, unless the battery's health is
//     below MinBatteryHealthPercent. After a preemption, work is accepted on
//     battery again only from BatteryMinPercent + BatteryResumeMargin.
//   - Lid: LidClosed without an external display sets BlankDisplay.
//   - Memory: MemPressure >= MemPressureLimit or SwapUsedMB > MemBudgetMB/2
//     sets Accept = false.
func Evaluate(p Policy, in Input, prev Decision) Decision {
	m := in.Metrics
	d := Decision{Since: prev.Since}
	var reasons []string

	// Battery.
	pct := m.BatteryPercent
	worn := m.BatteryHealthPercent > 0 && m.BatteryHealthPercent < MinBatteryHealthPercent
	chargeKnown := pct >= 0 && !worn
	d.Preempt = m.OnBattery && chargeKnown && pct < p.BatteryMinPercent
	switch {
	case !m.OnBattery || !chargeKnown:
		d.lowBattery = false
	case d.Preempt:
		d.lowBattery = true
	default:
		d.lowBattery = prev.lowBattery && pct < p.BatteryMinPercent+BatteryResumeMargin
	}
	if d.Preempt {
		reasons = append(reasons, fmt.Sprintf("battery %d%% < %d%%", pct, p.BatteryMinPercent))
	}

	// Heat.
	rose := prev.haveThrottle && m.ThrottleEvents > prev.lastThrottle
	d.haveThrottle, d.lastThrottle = true, m.ThrottleEvents
	if rose {
		d.throttleStreak = min(prev.throttleStreak+1, ThrottleSamples)
	}
	limit := m.CPUTempLimitC
	if !(limit > 0) || math.IsInf(limit, 0) {
		limit = p.MaxTempC
	}
	if !(limit > 0) || math.IsInf(limit, 0) {
		limit = DefaultMaxTempC
	}
	temp := m.CPUTempC
	tempKnown := temp > 0 && !math.IsInf(temp, 0)
	hot := tempKnown && temp >= limit
	throttling := d.throttleStreak >= ThrottleSamples
	cooled := (!tempKnown || temp <= limit-ResumeBelowC) && !rose
	d.thermal = hot || throttling || (prev.thermal && !cooled)
	d.Pause = d.thermal
	switch {
	case hot:
		reasons = append(reasons, fmt.Sprintf("CPU %.0f°C ≥ %.0f°C limit", temp, limit))
	case throttling:
		reasons = append(reasons, fmt.Sprintf("CPU thermal throttling (%d samples in a row)", ThrottleSamples))
	case d.thermal && rose:
		reasons = append(reasons, "cooling down: CPU still throttling")
	case d.thermal:
		reasons = append(reasons, fmt.Sprintf("cooling down: resuming at ≤ %.0f°C", limit-ResumeBelowC))
	}

	if m.OnBattery && !p.RunOnBattery {
		reasons = append(reasons, "on battery")
	} else if d.lowBattery && !d.Preempt {
		reasons = append(reasons, fmt.Sprintf("battery %d%%, waiting for %d%%", pct, p.BatteryMinPercent+BatteryResumeMargin))
	}

	// Memory. These reasons carry no live values, so they stay stable from
	// sample to sample (callers log reason changes).
	switch {
	case m.MemPressure >= MemPressureLimit:
		reasons = append(reasons, "memory pressure")
	case in.MemBudgetMB > 0 && m.SwapUsedMB > in.MemBudgetMB/2:
		reasons = append(reasons, "memory pressure (swap)")
	}

	d.Accept = len(reasons) == 0
	if !d.Accept {
		d.Reason = reasons[0]
	}
	d.BlankDisplay = m.LidClosed && !in.ExternalDisplay
	if prev.Since.IsZero() || d.Accept != prev.Accept || d.Pause != prev.Pause || d.Preempt != prev.Preempt {
		d.Since = m.Time
	}
	return d
}
