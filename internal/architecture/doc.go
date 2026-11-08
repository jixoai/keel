// Package architecture holds repository-level layering guards.
//
// It contains no production code: the tests here answer questions like "may
// package A depend on package B?" by inspecting the real import graph with
// `go list -deps`, so the layering rule is enforced by the test suite rather
// than by review alone. The rule itself is documented in docs/KERNEL.md.
package architecture
