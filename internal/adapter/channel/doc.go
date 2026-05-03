// Package channel groups the per-channel webhook adapters
// (Meta Cloud, future PSP/PIX, etc.). The package itself has no
// runtime symbols; it exists so the convention_test in this directory
// can scan every sub-package's BodyTenantAssociation implementation
// for the SecretScope-justification marker required by ADR 0075 rev 3
// (fail-closed sub-rule, F-12 follow-up).
package channel
