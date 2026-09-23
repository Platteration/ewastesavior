package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestDispatch(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := run([]string{"/usr/bin/savior-sse2", "version"}, &out, &errb); rc != 0 || !strings.HasPrefix(out.String(), "savior ") {
		t.Fatalf("build-variant name: rc=%d out=%q err=%q", rc, out.String(), errb.String())
	}
	out.Reset()
	if rc := run([]string{"savior", "help"}, &out, &errb); rc != 0 || strings.Contains(out.String(), "sandbox-exec") {
		t.Fatalf("help: rc=%d, hidden command listed: %q", rc, out.String())
	}
	if rc := run([]string{"savior", "nope"}, &out, &errb); rc != 2 {
		t.Fatalf("unknown command rc=%d", rc)
	}
	if !isCommand("ctl") || isCommand("sse2") {
		t.Fatal("isCommand")
	}
}
