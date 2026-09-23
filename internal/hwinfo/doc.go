// Package hwinfo collects a node's static hardware inventory, its
// hardware-derived identity and live metrics from /proc and /sys, and
// implements `savior info`. See docs/DESIGN.md sections 4, 10.1, 10.3, 10.4
// and 11.1.
//
// Every entry point takes a root directory. "" or "/" means the running
// system; anything else is a tree laid out like / (proc, sys, etc) that
// stands in for it. Tests, including other packages' tests, use such trees to
// fake hardware (see testdata/README). Reads are best-effort: a missing or
// malformed file leaves the corresponding field at its zero value and never
// fails the whole collection, because old firmware exposes a surprising
// variety of broken attributes.
//
// Reading the running system is supported only on Linux. On other systems
// the live entry points report ErrUnsupported, but fixture roots work
// everywhere.
package hwinfo
