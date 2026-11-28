package boardintegration

import (
	"context"
	"errors"

	"github.com/jixoai/keel/internal/pluginstore"
)

// Surface is the board plugin's independent domain store (change
// keel-plugin-split). The board's own pgadapter owns its schema and applies it
// during New; this adapter exposes the lifecycle the plugin kernel needs
// without teaching the kernel any board SQL or pgx type.
var _ pluginstore.DomainStore = (*Surface)(nil)

// Domain names the storage owner.
func (s *Surface) Domain() string { return "board" }

// Migrate is a no-op because New already ran the board's idempotent migrations
// (core, sync, audit) before returning a usable surface.
func (s *Surface) Migrate(context.Context) error {
	if s == nil {
		return errors.New("boardintegration: board surface is not open")
	}
	return nil
}

// Ready proves the board log can reach its store. An empty tenant is the
// default board; Head performs a real storage round trip (a live fold answers
// without one), so a dead pool is reported instead of assumed healthy.
func (s *Surface) Ready(ctx context.Context) error {
	if s == nil || s.board == nil {
		return errors.New("boardintegration: board log is not open")
	}
	_, err := s.board.Head("")
	return err
}
