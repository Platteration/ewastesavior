package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/platteration/ewastesavior/internal/display"
)

// The status file is world-readable (0644), so it must never carry the
// hive's pairing code, which redeems for an admin session (SPEC-RUNTIME-02).
func TestStatusJSONOmitsPairCode(t *testing.T) {
	cs := consoleStatus{StatusInfo: display.StatusInfo{Hive: &display.HivePanel{
		URLs: []string{"https://10.0.0.1:7700"}, Fingerprint: "sha256:feed", PairCode: "ZZTESTCD"}}}
	b, err := json.Marshal(cs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ZZTESTCD") {
		t.Fatalf("status JSON carries the pairing code: %s", b)
	}
	if !strings.Contains(string(b), "sha256:feed") {
		t.Fatalf("status JSON lost the hive panel: %s", b)
	}
}

// The agent writes the status file without the code; the root console
// reads the code from the hive's own 0600 file, and the status screen gets
// it in-process.
func TestConsoleReadsPairCodeFromHiveFile(t *testing.T) {
	dir := t.TempDir()
	panel := filepath.Join(dir, "hive-status.json")
	if err := os.WriteFile(panel, []byte(`{"urls":["https://10.0.0.1:7700"],"fingerprint":"sha256:feed",
		"pair_code":"ZZTESTCD","nodes_online":3,"persistent":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := fakeAgent(nil, 0, nil)
	a.opt.StatusFile = filepath.Join(dir, "status.json")
	a.opt.HivePanelFile = panel
	if si := a.statusInfo(); si.Hive == nil || si.Hive.PairCode != "ZZTESTCD" {
		t.Fatalf("status screen panel %+v", si.Hive)
	}
	a.writeStatus()
	raw, err := os.ReadFile(a.opt.StatusFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ZZTESTCD") || !strings.Contains(string(raw), "sha256:feed") {
		t.Fatalf("status file:\n%s", raw)
	}
	var out strings.Builder
	renderConsole(&out, a.opt.StatusFile, panel, time.Now())
	if !strings.Contains(out.String(), "pairing code ZZTESTCD") || !strings.Contains(out.String(), "sha256:feed") {
		t.Fatalf("console:\n%s", out.String())
	}
	// Whoever can't read the hive's file doesn't see a code.
	out.Reset()
	renderConsole(&out, a.opt.StatusFile, filepath.Join(dir, "unreadable"), time.Now())
	if strings.Contains(out.String(), "pairing code") || !strings.Contains(out.String(), "This machine is the hive") {
		t.Fatalf("console without the hive file:\n%s", out.String())
	}
}
