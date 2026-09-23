package runner

import "testing"

// checkTable compares a hand-written syscall table with x/sys/unix values.
func checkTable(t *testing.T, arch string, want map[string]uint32) {
	t.Helper()
	tab := syscallTable[arch]
	if len(tab) != len(want) {
		t.Errorf("%s table has %d entries, x/sys/unix list has %d", arch, len(tab), len(want))
	}
	for name, nr := range want {
		if got, ok := tab[name]; !ok || got != nr {
			t.Errorf("%s %s: table %d (present %v), unix %d", arch, name, got, ok, nr)
		}
	}
}
