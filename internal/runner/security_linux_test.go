package runner

import (
	"fmt"
	"os"
	"testing"
)

func TestIsolatedScratchFailsClosed(t *testing.T) {
	for _, mode := range []string{ModeStrict, ModeAuto} {
		if err := validateScratch(mode, false, 0); err == nil {
			t.Fatalf("%s accepted unbounded disk scratch", mode)
		}
		if err := validateScratch(mode, true, 1000); err == nil {
			t.Fatalf("%s accepted RAM scratch without mount privilege", mode)
		}
		if err := validateScratch(mode, true, 0); err != nil {
			t.Fatalf("%s refused bounded root RAM scratch: %v", mode, err)
		}
	}
	if err := validateScratch(ModeNone, false, 1000); err != nil {
		t.Fatalf("explicit trusted-command mode changed: %v", err)
	}
}

func TestDirectoryLimitBeforeFullEnumeration(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 300; i++ {
		if err := os.WriteFile(fmt.Sprintf("%s/file-%03d", dir, i), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if entries, err := boundedDirEntries(f, 255); err == nil || entries != nil {
		t.Fatalf("oversized directory must fail without returning retained entries: %v", err)
	}
	f.Close()
	f, err = os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entries, err := boundedDirEntries(f, 300)
	if err != nil || len(entries) != 300 {
		t.Fatalf("valid directory failed: entries=%d, err=%v", len(entries), err)
	}
}

func TestScratchRefusalPrecedesWorkRootCleanup(t *testing.T) {
	work := t.TempDir()
	sentinel := work + "/keep"
	if err := os.WriteFile(sentinel, []byte("existing files"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Runner{mode: ModeStrict, cfg: Config{WorkRoot: work}}
	if err := r.init(); err == nil {
		t.Fatal("runner accepted disk scratch")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "existing files" {
		t.Fatalf("failed startup changed WorkRoot: %q, %v", data, err)
	}
}
