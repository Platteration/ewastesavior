package hwinfo

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/platteration/ewastesavior/internal/proto"
)

func runInfo(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestMainText(t *testing.T) {
	cases := map[string][]string{
		"thinkpad-t60": {
			"Machine    LENOVO ThinkPad T60 (2007FVG), laptop, BIOS 10/23/2006",
			"CPU        Genuine Intel(R) CPU T2400 @ 1.83GHz, 2 logical / 2 physical cores, 1833 MHz",
			"flags: pae nx sse sse2 sse3 vmx",
			"eth0 00:16:41:e3:4a:7b pci e1000e wired up 1000 Mb/s",
			"LVDS-1 connected 1024x768 285x214 mm (built-in)",
			"Battery    84%, Charging, health 80%, on AC",
			"Sensors    CPU 52°C via coretemp, pause at 85°C, 5 thermal throttle events",
			"Node ID    n6ead4f09ab5f (from mac:001641e34a7b)",
			"Name       savior-e34a7b, identify code " + proto.ShortCode("n6ead4f09ab5f"),
			"Lid        open",
			"Roles      compute, display (suggested)",
			"laptop: stops taking tasks on battery",
		},
		"amd-desktop": {
			"Display    none (headless)",
			"Sensors    CPU 38°C via k10temp, pause at 70°C",
			"Node ID    n89c6cc3f885b (from uuid:4f6e2a10-8d3b-11de-8a39-0800200c9a66)",
			"Roles      compute (suggested)",
			"hive: a good candidate",
		},
		"atom-netbook": {
			"Battery    55%, Discharging, health 91%, ON BATTERY",
			"Lid        closed",
			"sdb 3819 MB usb HDD \"Cruzer Blade\" removable",
		},
		"p4-desktop": {"Sensors    CPU 49°C via acpitz, pause at 80°C, 24 thermal throttle events"},
		"qemu": {
			"Machine    QEMU Standard PC (i440FX + PIIX, 1996), virtual machine, BIOS 04/01/2014",
			"WARNING: no stable hardware ID",
			"Sensors    no CPU temperature sensor, pause at 85°C",
		},
	}
	for _, m := range machines {
		t.Run(m, func(t *testing.T) {
			code, out, errs := runInfo(t, "--root", fixture(t, m))
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errs)
			}
			for _, want := range cases[m] {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestMainJSON(t *testing.T) {
	code, out, errs := runInfo(t, "--json", "--max-temp", "60", "--root", fixture(t, "thinkpad-t60"))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var r Report
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out)
	}
	if r.Inventory.CPUModel != "Genuine Intel(R) CPU T2400 @ 1.83GHz" || r.Identity.NodeID != "n6ead4f09ab5f" ||
		r.Metrics.BatteryPercent != 84 || r.Metrics.CPUTempLimitC != 60 || r.Inventory.BenchScore != 0 {
		t.Fatalf("unexpected report: %+v", r)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal([]byte(out), &raw)
	if len(raw) != 3 || raw["inventory"] == nil || raw["identity"] == nil || raw["metrics"] == nil {
		t.Fatalf("top-level keys: %v", raw)
	}
}

func TestMainBench(t *testing.T) {
	if testing.Short() {
		t.Skip("benchmark takes 2 s")
	}
	code, out, _ := runInfo(t, "--json", "--bench", "--root", fixture(t, "qemu"))
	var r Report
	if code != 0 || json.Unmarshal([]byte(out), &r) != nil || r.Inventory.BenchScore < 1 {
		t.Fatalf("exit %d, bench %d", code, r.Inventory.BenchScore)
	}
}

func TestMainErrors(t *testing.T) {
	cases := []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"-h"}, 0, "usage: savior info"},
		{[]string{"--bogus"}, 2, "flag provided but not defined"},
		{[]string{"extra"}, 2, "usage: savior info"},
		{[]string{"--root", "/nonexistent/savior-root"}, 1, "Linux root"},
	}
	for _, c := range cases {
		code, _, errs := runInfo(t, c.args...)
		if code != c.code || !strings.Contains(errs, c.msg) {
			t.Errorf("run(%q) = %d, stderr %q; want %d containing %q", c.args, code, errs, c.code, c.msg)
		}
	}
}

func TestSuggestRoles(t *testing.T) {
	inv := proto.Inventory{Cores: 1, MemTotalMB: 256, CPUFlags: []string{"sse"}, IsLaptop: true}
	roles, notes := suggestRoles(inv, false)
	if strings.Join(roles, ",") != "compute" {
		t.Errorf("roles = %v", roles)
	}
	joined := strings.Join(notes, "\n")
	for _, want := range []string{"little RAM", "no SSE2", "laptop"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes lack %q: %v", want, notes)
		}
	}
	if strings.Contains(joined, "hive") {
		t.Errorf("a 256 MB laptop is no hive candidate: %v", notes)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[uint64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 5 << 20: "5.0 MiB", 3 << 40: "3.0 TiB"}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
