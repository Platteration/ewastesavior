package display

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	// Never touch the real /run/savior from tests.
	dir, err := os.MkdirTemp("", "display-test")
	if err != nil {
		panic(err)
	}
	backlightStateFile = filepath.Join(dir, "backlight.saved")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
