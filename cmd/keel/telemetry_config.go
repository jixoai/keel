package main

// Tenant telemetry configuration loading (change keel-plugin-telemetry, P0).
//
// `keel serve` used to hand the plugin only its flag/env-derived tenant
// document, which never contains a `telemetry` section: the plugin would stay
// inactive forever and OTLP would never leave the process. The admin API stores
// the tenant document in `tenants.config`; this loader applies the documented
// precedence:
//
//	boot defaults (flags/env) < stored tenant document < config.toml
//
// It is used both for the boot status snapshot and again at plugin Start, so a
// configuration written through PUT /v1/tenants/{id}/config is visible to the
// next enable without a process restart. A read/parse failure is returned: the
// plugin then fails closed (FAILED) instead of exporting with defaults the
// operator did not choose.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/configfile"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/servicehost"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/telemetry"
)

// telemetryConfigLoadTimeout bounds the stored-document read at activation and
// approval time; a database stall must not hang a plugin Start or an approval
// question.
const telemetryConfigLoadTimeout = 3 * time.Second

type tenantTelemetryLoader struct {
	documents  tenantConfigReader
	tenantID   uuid.UUID
	defaults   host.TenantConfig
	configFile *configfile.Loader
}

func newTenantTelemetryLoader(documents tenantConfigReader, tenantID uuid.UUID, defaults host.TenantConfig, configFile *configfile.Loader) tenantTelemetryLoader {
	return tenantTelemetryLoader{documents: documents, tenantID: tenantID, defaults: defaults, configFile: configFile}
}

// Load resolves the tenant's effective telemetry section.
func (l tenantTelemetryLoader) Load(ctx context.Context) (telemetry.Config, error) {
	tenant := l.defaults
	if l.documents != nil && l.tenantID != uuid.Nil {
		record, err := l.documents.GetTenantConfig(ctx, l.tenantID)
		switch {
		case err == nil:
			document, parseErr := host.ParseTenantConfig(record.Config)
			if parseErr != nil {
				return telemetry.Config{}, fmt.Errorf("stored tenant config: %w", parseErr)
			}
			tenant = host.Merge(l.defaults, document)
		case errors.Is(err, store.ErrNoTenant):
			// A tenant without a stored document runs on the boot defaults.
		default:
			return telemetry.Config{}, fmt.Errorf("read stored tenant config: %w", err)
		}
	}
	if l.configFile != nil {
		overlaid, applied, err := configfile.OverlayTenant(l.configFile.Pin(), tenant)
		if err != nil {
			return telemetry.Config{}, fmt.Errorf("config.toml telemetry: %w", err)
		}
		if applied {
			tenant = overlaid
		}
	}
	config := servicehost.TelemetryPluginConfig(tenant)
	config.TenantID = l.tenantID.String()
	return config, nil
}

// LoadBounded is the Start/approval entry point: it applies the load timeout.
func (l tenantTelemetryLoader) LoadBounded(ctx context.Context) (telemetry.Config, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), telemetryConfigLoadTimeout)
	defer cancel()
	return l.Load(loadCtx)
}
