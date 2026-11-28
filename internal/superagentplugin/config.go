// Package superagentplugin is the superagent plugin (change keel-plugin-split).
//
// The plugin owns its ticket/assignment/attention/outbox/subscription/cursor/
// report/lease rows through internal/superagentstore, declares
// `requires: [board]`, and reaches the task/channel/thread substrate only
// through the board plugin's Service interface. It never reads a board table
// and never imports a board store type. The legacy superagentboard bridge and
// mirror package is deleted; the writer below is the only integration and it is
// append-only on the board side.
package superagentplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"

	"github.com/jixoai/keel-board/protocol"
)

// DefaultChannel is the board channel mirrored tickets land in when the tenant
// document names none. Design §15.2 uses the same default.
const DefaultChannel = "superagent"

// BoardSource is the board link source and the `source` label value of every
// mirrored artifact.
const BoardSource = "superagent"

// SuperagentConfig is the tenant's `board.superagent` section. Both switches
// default to false: a tenant that never opts in gets no board writes.
type SuperagentConfig struct {
	// Mirror projects supervisor records into the board.
	Mirror bool `json:"mirror"`
	// Channel is the board channel (path) mirrored tickets use.
	Channel string `json:"channel"`
}

// DefaultSuperagentConfig returns the off-by-default section.
func DefaultSuperagentConfig() SuperagentConfig {
	return SuperagentConfig{Channel: DefaultChannel}
}

// Validate checks the channel grammar and fills the default. A malformed
// channel is a configuration error, never a silent fallback: writing a mirror
// into an unintended channel would expose supervisor text to its readers.
func (c SuperagentConfig) Validate() (SuperagentConfig, error) {
	channel := strings.Trim(strings.TrimSpace(c.Channel), "/")
	if channel == "" {
		channel = DefaultChannel
	}
	if err := protocol.ValidateChannel(channel); err != nil {
		return SuperagentConfig{}, fmt.Errorf("superagentplugin: board.superagent.channel: %w", err)
	}
	c.Channel = channel
	return c, nil
}

// Enabled reports whether board projection is on.
func (c SuperagentConfig) Enabled() bool { return c.Mirror }

// Document is the slice of the tenant configuration document this package
// reads. Other sections are ignored, so the document schema stays owned by its
// writers.
type Document struct {
	Board struct {
		Superagent SuperagentConfig `json:"superagent"`
	} `json:"board"`
}

// ParseDocument reads the `board.superagent` section of a tenant document. An
// absent section is the default (both switches off). Unknown fields are
// ignored, as with every other tenant section.
func ParseDocument(raw json.RawMessage) (SuperagentConfig, error) {
	config := DefaultSuperagentConfig()
	if len(strings.TrimSpace(string(raw))) == 0 {
		return config, nil
	}
	var document Document
	if err := json.Unmarshal(raw, &document); err != nil {
		return SuperagentConfig{}, fmt.Errorf("superagentplugin: tenant config is not valid JSON: %w", err)
	}
	config.Mirror = document.Board.Superagent.Mirror
	if strings.TrimSpace(document.Board.Superagent.Channel) != "" {
		config.Channel = document.Board.Superagent.Channel
	}
	return config.Validate()
}

// ConfigReader resolves one tenant's effective section. The mirror reads it
// every tick and the bridge reads it per request, so a stored document change
// needs no restart.
type ConfigReader interface {
	SuperagentConfig(ctx context.Context, tenantID uuid.UUID) (SuperagentConfig, error)
}

// StoreConfigReader reads the tenant document through keel's store. A tenant
// without a stored document runs the off-by-default section.
type StoreConfigReader struct {
	Store *store.Store
}

// SuperagentConfig implements ConfigReader.
func (r StoreConfigReader) SuperagentConfig(ctx context.Context, tenantID uuid.UUID) (SuperagentConfig, error) {
	if r.Store == nil {
		return SuperagentConfig{}, errors.New("superagentplugin: the config store is not wired")
	}
	if tenantID == uuid.Nil {
		return SuperagentConfig{}, errors.New("superagentplugin: a tenant is required")
	}
	record, err := r.Store.GetTenantConfig(ctx, tenantID)
	if errors.Is(err, store.ErrNoTenant) {
		return DefaultSuperagentConfig().Validate()
	}
	if err != nil {
		return SuperagentConfig{}, fmt.Errorf("superagentplugin: read tenant config: %w", err)
	}
	return ParseDocument(record.Config)
}

// StaticConfigReader is the test/deployment seam for one fixed section.
type StaticConfigReader struct {
	Config SuperagentConfig
	Err    error
}

// SuperagentConfig implements ConfigReader.
func (r StaticConfigReader) SuperagentConfig(context.Context, uuid.UUID) (SuperagentConfig, error) {
	if r.Err != nil {
		return SuperagentConfig{}, r.Err
	}
	if r.Config.Channel == "" {
		r.Config.Channel = DefaultChannel
	}
	return r.Config.Validate()
}
