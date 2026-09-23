package logging

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.log")
	r, err := OpenRotating(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		r.Write([]byte(strings.Repeat("a", 30) + "\n"))
	}
	r.Close()
	st, err := os.Stat(p)
	if err != nil || st.Size() > 100 || st.Mode().Perm() != 0o600 {
		t.Fatalf("current log: %v %v", st, err)
	}
	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatal("no rotated file")
	}
}

func TestParseLevel(t *testing.T) {
	if ParseLevel("DEBUG") != slog.LevelDebug || ParseLevel("warn") != slog.LevelWarn || ParseLevel("x") != slog.LevelInfo {
		t.Fatal("levels")
	}
}
