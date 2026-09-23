package hwinfo

import (
	"math"
	"strings"
	"time"
)

// Temperature plausibility (DESIGN 10.4): readings at or below 0 °C, at or
// above 125 °C, or unchanged for StuckSensorAfter are ignored.
const (
	minPlausibleC    = 0
	maxPlausibleC    = 125
	StuckSensorAfter = 10 * time.Minute
)

// Sensor-provided limits outside this range are ignored: a "max" of 0 or
// 20 °C is a firmware bug and would pause the node forever.
const (
	minSensorLimitC = 40
	maxSensorLimitC = 125
)

// cpuHwmonChips are the hwmon drivers that measure the CPU itself. Everything
// else (drivetemp, radeon, nouveau, iwlwifi, acpitz-as-hwmon ...) is ignored.
var cpuHwmonChips = map[string]bool{"coretemp": true, "k8temp": true, "k10temp": true, "via_cputemp": true}

// sensorReading is one CPU temperature input.
type sensorReading struct {
	key    string    // input file path; identifies the input for stuck detection
	chip   string    // hwmon chip name, or "acpitz"
	milliC int64     // raw reading in m°C
	limits []float64 // pause-threshold candidates (°C) from this input's max/crit
}

func (r sensorReading) celsius() float64 { return float64(r.milliC) / 1000 }

// plausible applies the fixed range check.
func (r sensorReading) plausible() bool {
	c := r.celsius()
	return c > minPlausibleC && c < maxPlausibleC
}

// sensorLimit converts a raw m°C limit attribute into a candidate, applying
// offset (e.g. -5 for crit). ok is false when missing or implausible.
func sensorLimit(p string, offsetC float64) (float64, bool) {
	v, ok := readInt(p)
	if !ok {
		return 0, false
	}
	c := float64(v)/1000 + offsetC
	if c < minSensorLimitC || c > maxSensorLimitC {
		return 0, false
	}
	return c, true
}

// readHwmonCPU reads temp*_input of the CPU hwmon chips. Old kernels keep
// name and temp files under hwmonN/device.
func readHwmonCPU(root string) []sensorReading {
	var out []sensorReading
	for _, hw := range listDir(rootPath(root, "sys/class/hwmon")) {
		base := rootPath(root, "sys/class/hwmon/"+hw)
		chip, ok := readAttr(base + "/name")
		if !ok {
			base += "/device"
			chip = attr(base + "/name")
		}
		if !cpuHwmonChips[chip] {
			continue
		}
		for _, f := range listDir(base) {
			idx, ok := strings.CutSuffix(f, "_input")
			if !ok {
				continue
			}
			if _, ok := numberedName(idx, "temp"); !ok {
				continue
			}
			v, ok := readInt(base + "/" + f)
			if !ok {
				continue
			}
			r := sensorReading{key: base + "/" + f, chip: chip, milliC: v}
			maxC, hasMax := sensorLimit(base+"/"+idx+"_max", 0)
			critC, hasCrit := sensorLimit(base+"/"+idx+"_crit", -5)
			switch {
			case chip == "k10temp" && hasMax:
				// DESIGN 10.4: on k10temp the limit is temp1_max; its crit
				// is the HTC point, which sits only a few degrees higher.
				r.limits = []float64{maxC}
			default:
				if hasMax {
					r.limits = append(r.limits, maxC)
				}
				if hasCrit {
					r.limits = append(r.limits, critC)
				}
			}
			out = append(out, r)
		}
	}
	return out
}

// readACPIZones reads ACPI thermal zones of type acpitz. A "critical" trip
// point contributes crit-5 and a "hot" trip point its own value as limits;
// passive and active trips are cooling hints, not limits.
func readACPIZones(root string) []sensorReading {
	var out []sensorReading
	for _, z := range listDir(rootPath(root, "sys/class/thermal")) {
		if _, ok := numberedName(z, "thermal_zone"); !ok {
			continue
		}
		dir := rootPath(root, "sys/class/thermal/"+z)
		if attr(dir+"/type") != "acpitz" {
			continue
		}
		v, ok := readInt(dir + "/temp")
		if !ok {
			continue
		}
		r := sensorReading{key: dir + "/temp", chip: "acpitz", milliC: v}
		for _, f := range listDir(dir) {
			tp, ok := strings.CutSuffix(f, "_type")
			if !ok || !strings.HasPrefix(tp, "trip_point_") {
				continue
			}
			var c float64
			switch attr(dir + "/" + f) {
			case "critical":
				c, ok = sensorLimit(dir+"/"+tp+"_temp", -5)
			case "hot":
				c, ok = sensorLimit(dir+"/"+tp+"_temp", 0)
			default:
				ok = false
			}
			if ok {
				r.limits = append(r.limits, c)
			}
		}
		out = append(out, r)
	}
	return out
}

// cpuTemp is the result of choosing a CPU temperature.
type cpuTemp struct {
	celsius float64 // hottest valid reading; 0 = unknown
	chip    string  // chip of that reading
	limits  []float64
}

// pickCPUTemp returns the hottest valid reading of the CPU hwmon chips, else
// of the ACPI zones (DESIGN 10.4). A group whose readings are all invalid
// (implausible or stuck) counts as absent. Limits come only from inputs
// with valid readings, so a broken zone can't lower the threshold.
func pickCPUTemp(groups [][]sensorReading, valid func(sensorReading) bool) cpuTemp {
	for _, g := range groups {
		var t cpuTemp
		for _, r := range g {
			if !valid(r) {
				continue
			}
			if c := r.celsius(); c > t.celsius {
				t.celsius, t.chip = c, r.chip
			}
			t.limits = append(t.limits, r.limits...)
		}
		if t.celsius > 0 {
			return t
		}
	}
	return cpuTemp{}
}

// tempLimit returns min(maxTempC, sensor limits). maxTempC <= 0 means no
// configured bound. 0 = unknown.
func tempLimit(maxTempC float64, limits []float64) float64 {
	limit := math.Inf(1)
	if maxTempC > 0 {
		limit = maxTempC
	}
	for _, l := range limits {
		limit = math.Min(limit, l)
	}
	if math.IsInf(limit, 1) {
		return 0
	}
	return limit
}

// sensorHistory remembers when an input's value last changed.
type sensorHistory struct {
	milliC int64
	since  time.Time
}

// stuckFilter tracks readings across samples. It returns the validity
// check for this sample and forgets inputs that have disappeared.
type stuckFilter struct {
	hist map[string]sensorHistory
}

// check updates the history with this sample's readings (all groups) and
// returns a function reporting whether a reading is plausible and has
// changed within StuckSensorAfter.
func (f *stuckFilter) check(now time.Time, groups [][]sensorReading) func(sensorReading) bool {
	if f.hist == nil {
		f.hist = map[string]sensorHistory{}
	}
	stuck := map[string]bool{}
	seen := map[string]bool{}
	for _, g := range groups {
		for _, r := range g {
			seen[r.key] = true
			h, ok := f.hist[r.key]
			if !ok || h.milliC != r.milliC || now.Before(h.since) {
				f.hist[r.key] = sensorHistory{milliC: r.milliC, since: now}
				continue
			}
			if now.Sub(h.since) >= StuckSensorAfter {
				stuck[r.key] = true
			}
		}
	}
	for k := range f.hist {
		if !seen[k] {
			delete(f.hist, k)
		}
	}
	return func(r sensorReading) bool { return r.plausible() && !stuck[r.key] }
}
