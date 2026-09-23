package hwinfo

import (
	"strings"
)

// maxSupplies caps the number of power supplies looked at.
const maxSupplies = 32

// supply is one /sys/class/power_supply entry.
type supply struct {
	name, typ, status                   string
	online                              int   // Mains: 1, 0, or -1 when unreadable
	present                             bool  // Battery: present != 0 (missing file = present)
	device                              bool  // scope=Device: a peripheral's battery (mouse, keyboard)
	capacity                            int   // percent; -1 = unknown
	energyNow, energyFull, energyDesign int64 // µWh; 0 = unknown
	chargeNow, chargeFull, chargeDesign int64 // µAh; 0 = unknown
}

// readSupplies reads /sys/class/power_supply.
func readSupplies(root string) []supply {
	var out []supply
	for _, name := range listDir(rootPath(root, "sys/class/power_supply")) {
		dir := "sys/class/power_supply/" + name + "/"
		s := supply{name: name, online: -1, capacity: -1}
		s.typ = attr(rootPath(root, dir+"type"))
		s.status = attr(rootPath(root, dir+"status"))
		s.device = strings.EqualFold(attr(rootPath(root, dir+"scope")), "Device")
		if v, ok := readInt(rootPath(root, dir+"online")); ok {
			s.online = int(v)
		}
		p, ok := readAttr(rootPath(root, dir+"present"))
		s.present = !ok || p != "0"
		if v, ok := readInt(rootPath(root, dir+"capacity")); ok && v >= 0 {
			s.capacity = int(min(v, 100))
		}
		pos := func(file string) int64 {
			v, ok := readInt(rootPath(root, dir+file))
			if !ok || v <= 0 {
				return 0
			}
			return v
		}
		if s.typ == "Battery" {
			s.energyNow, s.energyFull, s.energyDesign = pos("energy_now"), pos("energy_full"), pos("energy_full_design")
			s.chargeNow, s.chargeFull, s.chargeDesign = pos("charge_now"), pos("charge_full"), pos("charge_full_design")
		}
		out = append(out, s)
		if len(out) == maxSupplies {
			break
		}
	}
	return out
}

// isSystemBattery reports whether s powers this machine (not a peripheral,
// and physically present).
func (s supply) isSystemBattery() bool {
	return s.typ == "Battery" && !s.device && s.present
}

// percent returns the charge in percent: capacity, else energy_now /
// energy_full, else charge_now / charge_full. -1 = unknown.
func (s supply) percent() int {
	switch {
	case s.capacity >= 0:
		return s.capacity
	case s.energyFull > 0:
		return ratioPercent(s.energyNow, s.energyFull)
	case s.chargeFull > 0:
		return ratioPercent(s.chargeNow, s.chargeFull)
	}
	return -1
}

// health returns full / full_design in percent (energy, else charge), or 0
// when unknown.
func (s supply) health() int {
	switch {
	case s.energyFull > 0 && s.energyDesign > 0:
		return max(ratioPercent(s.energyFull, s.energyDesign), 1)
	case s.chargeFull > 0 && s.chargeDesign > 0:
		return max(ratioPercent(s.chargeFull, s.chargeDesign), 1)
	}
	return 0
}

// ratioPercent returns round(100*a/b) clamped to 0..100.
func ratioPercent(a, b int64) int {
	if b <= 0 || a <= 0 {
		return 0
	}
	if a >= b {
		return 100
	}
	return int((a*100 + b/2) / b)
}

// batteryState summarizes the power supplies (DESIGN 10.4).
type batteryState struct {
	present   bool
	onBattery bool
	percent   int // -1 = no battery or unknown
	health    int // 0 = unknown
	status    string
}

// summarizeBatteries applies the DESIGN 10.4 rules. OnBattery: if any Mains
// supply exists, true only when all of them report online=0; otherwise true
// only when a battery is Discharging. Additionally it is never true without
// a present system battery, so a desktop whose firmware exposes a dead AC
// adapter object doesn't stop taking work. Several batteries (ThinkPad
// ultrabay) are combined weighted by their full capacity.
func summarizeBatteries(supplies []supply) batteryState {
	st := batteryState{percent: -1}
	var bats []supply
	mains, mainsOffline := 0, 0
	for _, s := range supplies {
		switch {
		case s.typ == "Mains":
			mains++
			if s.online == 0 {
				mainsOffline++
			}
		case s.isSystemBattery():
			bats = append(bats, s)
		}
	}
	if len(bats) == 0 {
		return st
	}
	st.present = true
	discharging, charging := false, false
	for _, b := range bats {
		switch b.status {
		case "Discharging":
			discharging = true
		case "Charging":
			charging = true
		}
	}
	if mains > 0 {
		st.onBattery = mainsOffline == mains
	} else {
		st.onBattery = discharging
	}
	switch {
	case discharging:
		st.status = "Discharging"
	case charging:
		st.status = "Charging"
	default:
		st.status = bats[0].status
	}
	st.percent, st.health = combineBatteries(bats)
	return st
}

// combineBatteries merges per-battery percent and health. With one battery
// it's that battery's; with several, percent is weighted by full capacity
// when every battery reports it in the same unit, else a plain mean.
func combineBatteries(bats []supply) (percent, health int) {
	if len(bats) == 1 {
		return bats[0].percent(), bats[0].health()
	}
	weight := func(b supply) int64 { return 1 }
	switch {
	case allBatteries(bats, func(b supply) bool { return b.energyFull > 0 }):
		weight = func(b supply) int64 { return b.energyFull }
	case allBatteries(bats, func(b supply) bool { return b.chargeFull > 0 }):
		weight = func(b supply) int64 { return b.chargeFull }
	}
	var sum, wsum int64
	var hsum, hn int
	for _, b := range bats {
		if p := b.percent(); p >= 0 {
			sum += int64(p) * weight(b)
			wsum += weight(b)
		}
		if h := b.health(); h > 0 {
			hsum += h
			hn++
		}
	}
	percent = -1
	if wsum > 0 {
		percent = int((sum + wsum/2) / wsum)
	}
	if hn > 0 {
		health = (hsum + hn/2) / hn
	}
	return percent, health
}

func allBatteries(bats []supply, f func(supply) bool) bool {
	for _, b := range bats {
		if !f(b) {
			return false
		}
	}
	return true
}
