package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	channelinstagram "github.com/pericles-luz/crm/internal/adapter/channel/instagram"
	"github.com/pericles-luz/crm/internal/adapter/channels/instagram"
)

// InstagramIGBusinessIDLookup resolves a tenant's Instagram Business
// Account id — the reverse direction of ChannelAssociationLookup — for
// the OAuth callback's post-connect SubscribeApp call
// (channelinstagram.IGBusinessIDLookup). Mirrors
// instagramOutboundIGBusinessID (cmd/server/instagram_outbound_wire.go),
// which resolves the same (channel, tenant_id) -> association mapping
// for outbound sends; kept as a separate small type here since that one
// is a cmd/server-private helper the outbound wire already tests via its
// own boot-time seams.
type InstagramIGBusinessIDLookup struct {
	db PgxConn
}

// NewInstagramIGBusinessIDLookup returns a lookup bound to db.
func NewInstagramIGBusinessIDLookup(db PgxConn) *InstagramIGBusinessIDLookup {
	return &InstagramIGBusinessIDLookup{db: db}
}

const instagramIGBusinessIDSelectSQL = `
	SELECT association
	  FROM tenant_channel_associations
	 WHERE channel = $1 AND tenant_id = $2
	 LIMIT 1`

// IGBusinessID returns "" (no error) when the tenant has no Instagram
// association yet.
func (l *InstagramIGBusinessIDLookup) IGBusinessID(ctx context.Context, tenantID uuid.UUID) (string, error) {
	var igBusinessID string
	err := l.db.QueryRow(ctx, instagramIGBusinessIDSelectSQL, instagram.Channel, tenantID).Scan(&igBusinessID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lookup instagram ig_business_id: %w", err)
	}
	return igBusinessID, nil
}

// Compile-time assertion that IGBusinessID satisfies the OAuth callback
// handler's lookup port.
var _ channelinstagram.IGBusinessIDLookup = (*InstagramIGBusinessIDLookup)(nil)
