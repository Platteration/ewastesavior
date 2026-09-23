// Package ctl implements `savior ctl`, the operator command line for a
// SaviorOS hive (docs/DESIGN.md sections 6.3, 7.2, 7.3 and 8), and Client,
// a Go client for the hive's admin API that the CLI and the end-to-end
// tests share.
//
// Every connection to the hive is pinned to its certificate fingerprint.
// The first contact with an unknown hive learns the fingerprint (trust on
// first use), and every later request goes through a new client pinned to
// it. Logging in proves possession of the admin token without sending it:
// the proof is bound to the fingerprint the client saw, so it is useless
// to anyone else, and the hive proves in return that it knows the token
// too before the fingerprint is saved.
package ctl
