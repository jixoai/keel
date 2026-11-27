package boardintegration

// Provider gate (review P1-2). The board's sync subscription can start a
// platform thread for a new board root outside the outbox worker; a disabled
// or rotated entry must not reach a platform through that path either. The
// wrappers embed the concrete adapter, so every optional interface (edit,
// delete, reaction, files, typing, source read, links) is still promoted; only
// Receive/Post consult the durable gate.

import (
	"context"
	"errors"
	"sync"

	boardsync "github.com/jixoai/keel-board/sync"
	"github.com/jixoai/keel-board/sync/chatsdk"
	"github.com/jixoai/keel-board/sync/github"
	"github.com/jixoai/keel-board/sync/linear"
	"github.com/jixoai/keel-board/sync/slack"
)

var errSyncPaused = errors.New("boardintegration: sync connection is paused")

type runtimeGate struct {
	mu    sync.RWMutex
	check func(context.Context, boardsync.Key) error
}

func newRuntimeGate() *runtimeGate { return &runtimeGate{} }

func (g *runtimeGate) set(check func(context.Context, boardsync.Key) error) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.check = check
	g.mu.Unlock()
}

func (g *runtimeGate) err(ctx context.Context, key boardsync.Key) error {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	check := g.check
	g.mu.RUnlock()
	if check == nil {
		return nil
	}
	return check(ctx, key)
}

func wrapSyncProvider(provider boardsync.Provider, key boardsync.Key, gate *runtimeGate) boardsync.Provider {
	switch concrete := provider.(type) {
	case *slack.Provider:
		return &gatedSlack{Provider: concrete, key: key, gate: gate}
	case *github.Provider:
		return &gatedGitHub{Provider: concrete, key: key, gate: gate}
	case *linear.Provider:
		return &gatedLinear{Provider: concrete, key: key, gate: gate}
	case *chatsdk.Provider:
		return &gatedChatSDK{Provider: concrete, key: key, gate: gate}
	default:
		return provider
	}
}

type gatedSlack struct {
	*slack.Provider
	key  boardsync.Key
	gate *runtimeGate
}

func (p *gatedSlack) Receive(ctx context.Context, key boardsync.Key, req *boardsync.WebhookRequest) (boardsync.Intake, error) {
	if err := p.gate.err(ctx, p.key); err != nil {
		return boardsync.Intake{}, err
	}
	return p.Provider.Receive(ctx, key, req)
}

func (p *gatedSlack) Post(ctx context.Context, key boardsync.Key, target boardsync.ResolvedTarget, msg boardsync.OutboundMessage) (boardsync.Posted, error) {
	if err := p.gate.err(ctx, p.key); err != nil {
		return boardsync.Posted{}, err
	}
	return p.Provider.Post(ctx, key, target, msg)
}

type gatedGitHub struct {
	*github.Provider
	key  boardsync.Key
	gate *runtimeGate
}

func (p *gatedGitHub) Receive(ctx context.Context, key boardsync.Key, req *boardsync.WebhookRequest) (boardsync.Intake, error) {
	if err := p.gate.err(ctx, p.key); err != nil {
		return boardsync.Intake{}, err
	}
	return p.Provider.Receive(ctx, key, req)
}

func (p *gatedGitHub) Post(ctx context.Context, key boardsync.Key, target boardsync.ResolvedTarget, msg boardsync.OutboundMessage) (boardsync.Posted, error) {
	if err := p.gate.err(ctx, p.key); err != nil {
		return boardsync.Posted{}, err
	}
	return p.Provider.Post(ctx, key, target, msg)
}

type gatedLinear struct {
	*linear.Provider
	key  boardsync.Key
	gate *runtimeGate
}

func (p *gatedLinear) Receive(ctx context.Context, key boardsync.Key, req *boardsync.WebhookRequest) (boardsync.Intake, error) {
	if err := p.gate.err(ctx, p.key); err != nil {
		return boardsync.Intake{}, err
	}
	return p.Provider.Receive(ctx, key, req)
}

func (p *gatedLinear) Post(ctx context.Context, key boardsync.Key, target boardsync.ResolvedTarget, msg boardsync.OutboundMessage) (boardsync.Posted, error) {
	if err := p.gate.err(ctx, p.key); err != nil {
		return boardsync.Posted{}, err
	}
	return p.Provider.Post(ctx, key, target, msg)
}

type gatedChatSDK struct {
	*chatsdk.Provider
	key  boardsync.Key
	gate *runtimeGate
}

func (p *gatedChatSDK) Receive(ctx context.Context, key boardsync.Key, req *boardsync.WebhookRequest) (boardsync.Intake, error) {
	if err := p.gate.err(ctx, p.key); err != nil {
		return boardsync.Intake{}, err
	}
	return p.Provider.Receive(ctx, key, req)
}

func (p *gatedChatSDK) Post(ctx context.Context, key boardsync.Key, target boardsync.ResolvedTarget, msg boardsync.OutboundMessage) (boardsync.Posted, error) {
	if err := p.gate.err(ctx, p.key); err != nil {
		return boardsync.Posted{}, err
	}
	return p.Provider.Post(ctx, key, target, msg)
}
