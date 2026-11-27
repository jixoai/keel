package boardintegration

// Sync route provisioning (keel-board-v1 tasks 10.6/12.1, Phase 1 hardening
// P0-1).
//
// A sync entry's static route names the board channel a platform event is
// imported into. board's authorizer resolves a channel relation before it
// serves any content, so an entry that names a channel nobody owns imports
// successfully and is then invisible (404) to every reader. The platform actor
// of an imported message is `slack:...`/`github:...`/`linear:...`, not a keel
// Account, so the owner reconciler intentionally can't invent an owner for it.
//
// The deployment therefore requires the channel to exist and to carry at least
// one `board_channel` relation before the entry is accepted. Creating the
// channel first is the documented path: POST /v1/board/channels records the
// creator as owner. There is no implicit "grant everyone" fallback.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/store"

	boardlog "github.com/jixoai/keel-board/log"
)

// errSyncRouteUnprovisioned marks an entry whose static route names a board
// channel that is absent or has no relation tuple. The HTTP layer answers 400
// with the actionable message; it is never stored and never silently skipped.
var errSyncRouteUnprovisioned = errors.New("boardintegration: the sync route target is not provisioned")

// routeACLReport is the provisioning state of every distinct board channel a
// route map names. A route map with no board channels is ready.
type routeACLReport struct {
	// Missing channels do not exist in the board projection.
	Missing []string
	// Orphaned channels exist but have no relation of any type, so even a
	// board.manage caller would be refused by the content authorizer.
	Orphaned []string
	// Unknown is set when the board projection or the relation store could not
	// be read. The report is not ready and the reason is bounded.
	Unknown string
}

// Ready reports whether every route target can serve content.
func (r routeACLReport) Ready() bool {
	return r.Unknown == "" && len(r.Missing) == 0 && len(r.Orphaned) == 0
}

// Error returns the operator-facing reason. It names channels and the exact
// create call; it never contains a credential or a message body.
func (r routeACLReport) Error() string {
	parts := make([]string, 0, 2)
	for _, channel := range r.Missing {
		parts = append(parts, fmt.Sprintf(
			"route target %q does not exist; create it first with POST /v1/board/channels {\"path\":%q} (the creator becomes the channel owner)",
			channel, channel))
	}
	for _, channel := range r.Orphaned {
		parts = append(parts, fmt.Sprintf(
			"route target %q exists but has no ACL relation; POST /v1/board/channels {\"path\":%q} as a board.manage holder to adopt the orphan channel, or grant a board_channel viewer/editor relation",
			channel, channel))
	}
	if r.Unknown != "" {
		parts = append(parts, r.Unknown)
	}
	return strings.Join(parts, "; ")
}

// routeChannels returns the distinct board channels a route map targets, sorted
// so the error message and tests are deterministic.
func routeChannels(routes map[string]string) []string {
	seen := make(map[string]bool, len(routes))
	out := make([]string, 0, len(routes))
	for _, channel := range routes {
		channel = strings.Trim(strings.TrimSpace(channel), "/")
		if channel == "" || seen[channel] {
			continue
		}
		seen[channel] = true
		out = append(out, channel)
	}
	sort.Strings(out)
	return out
}

// checkRouteACL reads the board projection and the relation store for every
// route target. It refreshes the projection first: a channel created through
// another replica must be visible to this process before the entry is accepted.
func checkRouteACL(ctx context.Context, st *store.Store, board *boardlog.Board, tenantID uuid.UUID, routes map[string]string) (routeACLReport, error) {
	channels := routeChannels(routes)
	if len(channels) == 0 {
		return routeACLReport{}, nil
	}
	if st == nil || tenantID == uuid.Nil {
		return routeACLReport{Unknown: "the entry tenant or identity store is not wired"}, nil
	}
	if board == nil {
		return routeACLReport{Unknown: "the board projection is not wired"}, nil
	}
	if err := board.Refresh(ctx, tenantID.String()); err != nil {
		return routeACLReport{Unknown: "the board projection could not be read"}, err
	}
	var report routeACLReport
	readErr := board.Read(tenantID.String(), func(snapshot *boardlog.Snapshot) error {
		for _, channel := range channels {
			if snapshot.Channel(channel) == nil {
				report.Missing = append(report.Missing, channel)
				continue
			}
			relations, err := st.ListResourceRelations(ctx, tenantID, BoardChannelResourceType, channel, 1)
			if err != nil {
				return err
			}
			if len(relations) == 0 {
				report.Orphaned = append(report.Orphaned, channel)
			}
		}
		return nil
	})
	if readErr != nil {
		return routeACLReport{Unknown: "the route ACL could not be read"}, readErr
	}
	return report, nil
}

// validateRouteACL is the write-path check: a create/update must fail 400 with
// an actionable message rather than persist an entry that could never serve.
func (m *syncManager) validateRouteACL(ctx context.Context, tenantID uuid.UUID, routes map[string]string) error {
	if m == nil {
		return nil
	}
	report, err := checkRouteACL(ctx, m.store, m.board, tenantID, routes)
	if err != nil {
		return fmt.Errorf("%w: %s", errSyncRouteUnprovisioned, report.Error())
	}
	if !report.Ready() {
		return fmt.Errorf("%w: %s", errSyncRouteUnprovisioned, report.Error())
	}
	return nil
}
