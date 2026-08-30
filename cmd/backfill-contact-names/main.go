// backfill-contact-names is a one-shot maintenance tool: it resolves a
// real display name for every Messenger/Instagram contact created
// before the profile-fetch fix landed (see
// internal/adapter/channel/{messenger,instagram}/profile.go), i.e. every
// contact whose display_name still equals the raw PSID/IGSID Meta
// assigned it. New contacts already get a real name automatically going
// forward — this tool only touches pre-existing ones.
//
// Safety rails (see internal/adapter/db/postgres/contactbackfill for the
// query/update semantics):
//
//   - -apply defaults to false: the tool fetches and logs every
//     candidate's resolved name but writes nothing until you pass
//     -apply. Run without it first, review the report, then re-run
//     with -apply.
//   - Every UPDATE is guarded on display_name still equalling the raw
//     external id at write time, so a manual rename an operator made in
//     the meantime is never clobbered — it just logs as skipped.
//   - Calls the same best-effort, single-attempt ProfileFetcher PR #277
//     wired into production, with a fixed delay between calls
//     (-delay) so a large backlog doesn't hammer Meta's rate limits.
//
// Configuration:
//
//	MASTER_OPS_DATABASE_URL   mandatory — app_master_ops DSN (the same
//	                          env var every other cmd/server master-ops
//	                          consumer reads). Used for the
//	                          contact/contact_channel_identity scan+update
//	                          (internal/adapter/db/postgres/contactbackfill).
//	DATABASE_URL              mandatory — app_runtime DSN. Used ONLY to
//	                          read instagram_oauth_tokens: that table
//	                          grants SELECT to app_runtime, not
//	                          app_master_ops (it has no RLS — "composition
//	                          root/admin-flow config, not tenant runtime
//	                          data", per its own doc comment), so it must
//	                          be read the same way
//	                          cmd/server/instagram_outbound_wire.go reads
//	                          it — NOT through the master-ops pool.
//	META_MESSENGER_GRAPH_TOKEN / META_GRAPH_TOKEN   Messenger token,
//	                          same fallback precedence as
//	                          cmd/server/messenger_wire.go. Messenger
//	                          candidates are skipped (logged) when unset.
//	META_INSTAGRAM_GRAPH_TOKEN   Instagram global fallback token; a
//	                          tenant's own OAuth token (instagram_oauth_tokens)
//	                          is preferred when present, mirroring
//	                          cmd/server/instagram_wire.go.
//
// Flags: -apply, -tenant <uuid>, -channel messenger|instagram, -limit N,
// -delay <duration> (default 300ms).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	channelinstagram "github.com/pericles-luz/crm/internal/adapter/channel/instagram"
	channelmessenger "github.com/pericles-luz/crm/internal/adapter/channel/messenger"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/contactbackfill"
	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := loadConfig(os.Args[1:], os.Getenv)
	if err != nil {
		logger.Error("backfill-contact-names: config", "err", err.Error())
		os.Exit(1)
	}
	if err := run(context.Background(), logger, cfg, os.Getenv); err != nil {
		logger.Error("backfill-contact-names: exit", "err", err.Error())
		os.Exit(1)
	}
}

// config bundles everything loadConfig parses from flags. Extracted so
// unit tests can drive every flag-parsing path without touching a real
// database.
type config struct {
	masterDSN  string
	runtimeDSN string
	apply      bool
	tenant     uuid.UUID // uuid.Nil means every tenant
	channel    string    // "" means both messenger and instagram
	limit      int
	delay      time.Duration
}

// loadConfig parses args (excluding argv[0]) and reads MASTER_OPS_DATABASE_URL
// and DATABASE_URL from getenv. Returns a descriptive error so a
// misconfigured run surfaces with the exact knob name.
func loadConfig(args []string, getenv func(string) string) (config, error) {
	fs := flag.NewFlagSet("backfill-contact-names", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "write resolved names to the database (default: dry-run report only)")
	tenantFlag := fs.String("tenant", "", "restrict to a single tenant id (uuid)")
	channel := fs.String("channel", "", "restrict to \"messenger\" or \"instagram\" (default: both)")
	limit := fs.Int("limit", 0, "stop after touching this many candidates (0 = no limit)")
	delay := fs.Duration("delay", 300*time.Millisecond, "delay between Graph API calls")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	var tenantID uuid.UUID
	if strings.TrimSpace(*tenantFlag) != "" {
		id, err := uuid.Parse(*tenantFlag)
		if err != nil {
			return config{}, fmt.Errorf("-tenant: invalid uuid %q: %w", *tenantFlag, err)
		}
		tenantID = id
	}
	switch *channel {
	case "", "messenger", "instagram":
	default:
		return config{}, fmt.Errorf("-channel: must be \"messenger\", \"instagram\", or empty, got %q", *channel)
	}

	dsn := strings.TrimSpace(getenv("MASTER_OPS_DATABASE_URL"))
	if dsn == "" {
		return config{}, errors.New("MASTER_OPS_DATABASE_URL is required")
	}
	runtimeDSN := strings.TrimSpace(getenv("DATABASE_URL"))
	if runtimeDSN == "" {
		return config{}, errors.New("DATABASE_URL is required")
	}

	return config{
		masterDSN:  dsn,
		runtimeDSN: runtimeDSN,
		apply:      *apply,
		tenant:     tenantID,
		channel:    *channel,
		limit:      *limit,
		delay:      *delay,
	}, nil
}

func run(ctx context.Context, logger *slog.Logger, cfg config, getenv func(string) string) error {
	masterPool, err := pgxpool.New(ctx, cfg.masterDSN)
	if err != nil {
		return fmt.Errorf("pgxpool.New master: %w", err)
	}
	defer masterPool.Close()

	runtimePool, err := pgxpool.New(ctx, cfg.runtimeDSN)
	if err != nil {
		return fmt.Errorf("pgxpool.New runtime: %w", err)
	}
	defer runtimePool.Close()

	store, err := contactbackfill.New(masterPool)
	if err != nil {
		return fmt.Errorf("contactbackfill.New: %w", err)
	}

	var msgFetcher messengerProfileFetcher
	if token := messengerGraphToken(getenv); token != "" {
		f, err := channelmessenger.NewProfileFetcher(token)
		if err != nil {
			logger.Warn("backfill-contact-names: messenger fetcher disabled", "err", err.Error())
		} else {
			msgFetcher = f
		}
	} else {
		logger.Warn("backfill-contact-names: messenger fetcher disabled (no META_MESSENGER_GRAPH_TOKEN / META_GRAPH_TOKEN) — messenger candidates will be skipped")
	}

	// instagram_oauth_tokens has no RLS and grants SELECT to app_runtime
	// only (not app_master_ops — see the file doc comment), so it MUST be
	// read through the runtime pool, mirroring
	// cmd/server/instagram_outbound_wire.go exactly.
	tokenStore := pgstore.NewInstagramOAuthTokenStore(runtimePool)
	igLookup := channelinstagram.TokenLookup(func(ctx context.Context, tenantID uuid.UUID) (string, error) {
		accessToken, _, ok, err := tokenStore.Get(ctx, tenantID)
		if err != nil {
			return "", err
		}
		if ok && accessToken != "" {
			return accessToken, nil
		}
		return instagramGraphToken(getenv), nil
	})
	igFetcher, err := channelinstagram.NewProfileFetcher(igLookup)
	if err != nil {
		return fmt.Errorf("channelinstagram.NewProfileFetcher: %w", err)
	}

	sum, err := runWith(ctx, logger, cfg, store, msgFetcher, igFetcher)
	if err != nil {
		return err
	}
	logger.Info("backfill-contact-names: done",
		"apply", cfg.apply,
		"scanned", sum.Scanned,
		"resolved", sum.Resolved,
		"applied", sum.Applied,
		"would_apply", sum.WouldApply,
		"skipped_no_name", sum.SkippedNoName,
		"skipped_no_fetcher", sum.SkippedNoFetcher,
		"skipped_changed", sum.SkippedChanged,
		"errors", sum.Errors)
	return nil
}

// messengerGraphToken mirrors cmd/server/messenger_wire.go's
// messengerOutboundGraphToken precedence — kept as a small local copy
// since this binary cannot import another main package.
func messengerGraphToken(getenv func(string) string) string {
	if token := strings.TrimSpace(getenv("META_MESSENGER_GRAPH_TOKEN")); token != "" {
		return token
	}
	return strings.TrimSpace(getenv("META_GRAPH_TOKEN"))
}

// instagramGraphToken mirrors cmd/server/instagram_outbound_wire.go's
// instagramOutboundGraphToken — the global fallback used when a tenant
// has no per-tenant OAuth token on file.
func instagramGraphToken(getenv func(string) string) string {
	return strings.TrimSpace(getenv("META_INSTAGRAM_GRAPH_TOKEN"))
}

// messengerProfileFetcher and instagramProfileFetcher are the narrow
// ports runWith depends on, satisfied by
// channelmessenger.ProfileFetcher / channelinstagram.ProfileFetcher in
// production and by fakes in tests.
type messengerProfileFetcher interface {
	FetchDisplayName(ctx context.Context, psid string) (string, error)
}

type instagramProfileFetcher interface {
	FetchDisplayName(ctx context.Context, tenantID uuid.UUID, igsid string) (string, error)
}

// backfillStore is the narrow port runWith depends on, satisfied by
// *contactbackfill.Store in production and by a fake in tests.
type backfillStore interface {
	ListCandidates(ctx context.Context, channel string) ([]contactbackfill.Candidate, error)
	UpdateDisplayName(ctx context.Context, tenantID, contactID uuid.UUID, oldExternalID, newName string) (bool, error)
}

// summary tallies one run's outcome for the final log line.
type summary struct {
	Scanned          int
	Resolved         int
	Applied          int
	WouldApply       int
	SkippedNoName    int
	SkippedNoFetcher int
	SkippedChanged   int
	Errors           int
}

// runWith is the testable boundary: production wires the real
// pgxpool-backed store and Graph fetchers; tests pass in fakes.
func runWith(ctx context.Context, logger *slog.Logger, cfg config, store backfillStore, msgFetcher messengerProfileFetcher, igFetcher instagramProfileFetcher) (summary, error) {
	candidates, err := store.ListCandidates(ctx, cfg.channel)
	if err != nil {
		return summary{}, fmt.Errorf("list candidates: %w", err)
	}

	var sum summary
	for _, c := range candidates {
		if cfg.tenant != uuid.Nil && c.TenantID != cfg.tenant {
			continue
		}
		if cfg.limit > 0 && sum.Scanned >= cfg.limit {
			break
		}
		sum.Scanned++

		name, err := fetchName(ctx, c, msgFetcher, igFetcher)
		if err != nil || strings.TrimSpace(name) == "" {
			if err == errNoFetcher {
				sum.SkippedNoFetcher++
			} else {
				sum.SkippedNoName++
			}
			logger.Debug("backfill-contact-names: no name resolved",
				"tenant_id", c.TenantID, "contact_id", c.ContactID, "channel", c.Channel, "err", err)
			time.Sleep(cfg.delay)
			continue
		}
		sum.Resolved++

		if !cfg.apply {
			sum.WouldApply++
			logger.Info("backfill-contact-names: would apply",
				"tenant_id", c.TenantID, "contact_id", c.ContactID, "channel", c.Channel,
				"old", c.ExternalID, "new", name)
			time.Sleep(cfg.delay)
			continue
		}

		updated, err := store.UpdateDisplayName(ctx, c.TenantID, c.ContactID, c.ExternalID, name)
		switch {
		case err != nil:
			sum.Errors++
			logger.Error("backfill-contact-names: update failed",
				"tenant_id", c.TenantID, "contact_id", c.ContactID, "err", err.Error())
		case updated:
			sum.Applied++
			logger.Info("backfill-contact-names: applied",
				"tenant_id", c.TenantID, "contact_id", c.ContactID, "channel", c.Channel,
				"old", c.ExternalID, "new", name)
		default:
			sum.SkippedChanged++
			logger.Info("backfill-contact-names: skipped (changed since scan)",
				"tenant_id", c.TenantID, "contact_id", c.ContactID)
		}
		time.Sleep(cfg.delay)
	}
	return sum, nil
}

// errNoFetcher marks the "this channel has no fetcher wired" case
// distinctly from "the fetcher tried and found nothing" so the summary
// can report them separately.
var errNoFetcher = errors.New("backfill-contact-names: no fetcher configured for this channel")

// fetchName dispatches to the matching fetcher for c.Channel.
func fetchName(ctx context.Context, c contactbackfill.Candidate, msgFetcher messengerProfileFetcher, igFetcher instagramProfileFetcher) (string, error) {
	switch c.Channel {
	case "messenger":
		if msgFetcher == nil {
			return "", errNoFetcher
		}
		return msgFetcher.FetchDisplayName(ctx, c.ExternalID)
	case "instagram":
		if igFetcher == nil {
			return "", errNoFetcher
		}
		return igFetcher.FetchDisplayName(ctx, c.TenantID, c.ExternalID)
	default:
		return "", fmt.Errorf("unknown channel %q", c.Channel)
	}
}
