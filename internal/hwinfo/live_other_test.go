//go:build !linux

package hwinfo

import (
	"errors"
	"testing"
)

func TestLiveUnsupported(t *testing.T) {
	if _, err := Collect(""); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Collect: %v", err)
	}
	if _, err := GetIdentity("/"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("GetIdentity: %v", err)
	}
}
