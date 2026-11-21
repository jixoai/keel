// Package access is Keel's pure authorization core
// (change keel-bot-identity-access, A3).
//
// It owns one thing: deciding whether a principal may perform a permission-key
// action on a resource. The package is deliberately pure:
//
//   - the permission-key catalog is code, not a table (A1 decision 9);
//   - grants and relation tuples are read through the GrantSource and
//     RelationSource interfaces, so nothing here imports internal/store, HTTP,
//     the tool runtime or a UI package;
//   - a decision carries a machine-readable reason (missing_grant,
//     missing_relation, unknown_key, ...) for 403 copy and audit.
//
// The model is additive only. Effective permissions are the union of direct
// account grants and every group grant, taking the widest scope per key. There
// is no deny and no nested group (L5). Resource access additionally requires a
// fixed ReBAC-lite relation (owner/team_member/viewer/editor), and no relation
// always fails closed (L3).
//
// A4 wires this package to HTTP, tools, CLI and UI; A3 does not.
package access
