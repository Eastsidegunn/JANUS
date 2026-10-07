// Package worldtest provides test-only fakes for the core/world contract.
//
// These fakes do not execute OCI containers, create overlay mounts, isolate a
// network, or run shell commands. Their results are not evidence for any
// FR-SBX acceptance criterion. boundarylint forbids importing this package from
// production .go files; only _test.go consumers may use it.
package worldtest
