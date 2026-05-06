// Package slugreservation enforces F46 (SIN-62244): subdomain takeover
// defense via slug reservation, redirect window, and audited master
// override.
//
// Three pieces, one boundary:
//
//  1. Reservation: when a tenant releases a slug (deleted, slug changed,
//     suspended >30d) the slug is locked for 365 days. RequireSlugAvailable
//     refuses 409 on any creation/change attempt that hits an active
//     reservation. Hex layout: domain types and the use-case Service live
//     here; the Postgres implementations of Store / RedirectStore live in
//     internal/adapter/store/postgres.
//
//  2. Redirect window: requests to <old-slug>.<primary> answer 301 to
//     <new-slug>.<primary> with Clear-Site-Data: "cookies" for 12 months.
//     The redirect handler in this package serves that.
//
//  3. Master override: POST /api/master/slug-reservations/:slug/release
//     is the audited trapdoor. It runs through RequireMaster (and once
//     SIN-62223 lands, RequireMasterMFA), soft-deletes the reservation,
//     emits a structured master_slug_reservation_overridden audit event,
//     and posts an immediate Slack alert. See ADR 0079 §4 for the gating
//     flag.
//
// The package never imports database/sql or pgx; all storage flows
// through the Store / RedirectStore ports. HTTP handlers live alongside
// the use-cases because they are 1:1 with the deliverable, but they
// only consume the Service via its public API — they do not touch the
// store directly.
package slugreservation
