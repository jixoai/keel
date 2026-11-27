package boardintegration

// Sync entry configuration, validation, and provider assembly (keel-board-v1
// tasks 10.6 and 12.1).
//
// A stored entry names a platform, a role -> tenant-secret-name map, a small
// platform settings object, routes, and start channels. This file is the only
// place a provider is constructed: every credential comes from
// SecretResolver, never from the request body, a log or the entry row.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"

	"github.com/jixoai/keel-board/protocol"
	boardsync "github.com/jixoai/keel-board/sync"
	"github.com/jixoai/keel-board/sync/chatsdk"
	"github.com/jixoai/keel-board/sync/github"
	"github.com/jixoai/keel-board/sync/linear"
	"github.com/jixoai/keel-board/sync/slack"
)

// Supported sync platforms. The names match the board adapters' Platform
// values; chatsdk entries register under their sidecar's own platform, so the
// stored Platform is "chatsdk" and the provider's Platform() is the sidecar
// name.
const (
	syncPlatformSlack   = "slack"
	syncPlatformGitHub  = "github"
	syncPlatformLinear  = "linear"
	syncPlatformChatSDK = "chatsdk"
)

var syncPlatforms = map[string]bool{
	syncPlatformSlack:   true,
	syncPlatformGitHub:  true,
	syncPlatformLinear:  true,
	syncPlatformChatSDK: true,
}

// SyncEntryInput is one create/update document. It is the only request shape:
// there is no `secret` or `token` field, and the strict decoder rejects one.
// SecretRef values are tenant secret NAMES.
type SyncEntryInput struct {
	Key       string            `json:"key"`
	Platform  string            `json:"platform"`
	SecretRef map[string]string `json:"secretRef,omitempty"`
	// SecretRefSnake is the design document's `secret_ref` spelling. Both
	// names are accepted; an entry that uses both is refused rather than
	// guessing which wins.
	SecretRefSnake map[string]string `json:"secret_ref,omitempty"`
	Settings       json.RawMessage   `json:"settings,omitempty"`
	Routes         map[string]string `json:"routes,omitempty"`
	StartChannels  []string          `json:"startChannels,omitempty"`
	Enabled        *bool             `json:"enabled,omitempty"`
}

// SyncEntryPatch is the PATCH document. Every field is optional; the pointer
// distinguishes "omitted" from "set to empty". A change to anything except
// Enabled is structural and requires a restart (the response says so).
type SyncEntryPatch struct {
	Platform       *string            `json:"platform,omitempty"`
	SecretRef      *map[string]string `json:"secretRef,omitempty"`
	SecretRefSnake *map[string]string `json:"secret_ref,omitempty"`
	Settings       *json.RawMessage   `json:"settings,omitempty"`
	Routes         *map[string]string `json:"routes,omitempty"`
	StartChannels  *[]string          `json:"startChannels,omitempty"`
	Enabled        *bool              `json:"enabled,omitempty"`
}

// secretRefOf picks the one spelling a request used. Both is a 400, not a
// silent merge.
func (i SyncEntryInput) secretRef() (map[string]string, error) {
	if i.SecretRef != nil && i.SecretRefSnake != nil {
		return nil, errors.New("secretRef and secret_ref can't both be set")
	}
	if i.SecretRefSnake != nil {
		return i.SecretRefSnake, nil
	}
	return i.SecretRef, nil
}

func (p SyncEntryPatch) secretRef() (*map[string]string, error) {
	if p.SecretRef != nil && p.SecretRefSnake != nil {
		return nil, errors.New("secretRef and secret_ref can't both be set")
	}
	if p.SecretRefSnake != nil {
		return p.SecretRefSnake, nil
	}
	return p.SecretRef, nil
}

// SyncEntryView is the management projection. It never carries a credential
// value: credential is a status, and secretRef names are configuration.
type SyncEntryView struct {
	Key             string            `json:"key"`
	Platform        string            `json:"platform"`
	Enabled         bool              `json:"enabled"`
	SecretRef       map[string]string `json:"secretRef"`
	Settings        json.RawMessage   `json:"settings,omitempty"`
	Routes          map[string]string `json:"routes,omitempty"`
	StartChannels   []string          `json:"startChannels,omitempty"`
	Revision        int64             `json:"revision"`
	Credential      CredentialStatus  `json:"credential"`
	RestartRequired bool              `json:"restartRequired,omitempty"`
	CreatedAt       string            `json:"createdAt,omitempty"`
	UpdatedAt       string            `json:"updatedAt,omitempty"`
}

// SyncEntryResult is one create/update response. restartRequired is true when
// the running registry/workers still serve the previous provider set; the
// entry is disabled in the board sync store until the process restarts, so a
// pending change is a safe failure rather than a stale credential in use.
type SyncEntryResult struct {
	Entry           SyncEntryView `json:"entry"`
	RestartRequired bool          `json:"restartRequired"`
	PendingRestart  bool          `json:"pendingRestart,omitempty"`
}

func viewOf(record store.BoardSyncEntryRecord, status CredentialStatus) SyncEntryView {
	settings := record.Settings
	if len(settings) == 0 {
		settings = json.RawMessage("{}")
	}
	return SyncEntryView{
		Key: string(record.Key), Platform: record.Platform, Enabled: record.Enabled,
		SecretRef: nonNilMap(record.SecretRef), Settings: settings,
		Routes: nonNilMap(record.Routes), StartChannels: record.StartChannels,
		Revision: record.Revision, Credential: status,
		CreatedAt: formatTime(record.CreatedAt), UpdatedAt: formatTime(record.UpdatedAt),
	}
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func nonNilMap(value map[string]string) map[string]string {
	if value == nil {
		return map[string]string{}
	}
	return value
}

// parseSyncEntryInput decodes a create body strictly. Unknown fields are
// refused: a client that sends a plaintext secret by mistake gets a 400 instead
// of a silently stored token.
func parseSyncEntryInput(body []byte) (SyncEntryInput, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var input SyncEntryInput
	if err := decoder.Decode(&input); err != nil {
		return SyncEntryInput{}, fmt.Errorf("invalid sync entry body: %w", err)
	}
	return input, nil
}

func parseSyncEntryPatch(body []byte) (SyncEntryPatch, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var patch SyncEntryPatch
	if err := decoder.Decode(&patch); err != nil {
		return SyncEntryPatch{}, fmt.Errorf("invalid sync entry patch: %w", err)
	}
	return patch, nil
}

// validateSyncEntry checks an entry document without touching the secret store
// or the network. It fills defaults (routes/start channels/secretRef maps) and
// returns a normalized record.
func validateSyncEntry(record *store.BoardSyncEntryRecord) error {
	record.Key = strings.TrimSpace(record.Key)
	record.Platform = strings.ToLower(strings.TrimSpace(record.Platform))
	if !boardsync.Key(record.Key).Valid() {
		return fmt.Errorf("key: %q is not a sync key", record.Key)
	}
	if !syncPlatforms[record.Platform] {
		return fmt.Errorf("platform: %q is not slack, github, linear, or chatsdk", record.Platform)
	}
	if record.SecretRef == nil {
		record.SecretRef = map[string]string{}
	}
	normalized := make(map[string]string, len(record.SecretRef))
	for role, name := range record.SecretRef {
		role = strings.TrimSpace(role)
		name = strings.TrimSpace(name)
		normalized[role] = name
	}
	record.SecretRef = normalized
	if err := ValidateSecretRef(record.SecretRef); err != nil {
		return err
	}
	known := knownCredentialRoles(record.Platform)
	for role := range record.SecretRef {
		if !known[role] {
			return fmt.Errorf("secretRef.%s: unknown credential role for %s", role, record.Platform)
		}
	}
	if len(record.Settings) == 0 {
		record.Settings = json.RawMessage("{}")
	}
	if trimmed := bytes.TrimSpace(record.Settings); len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return errors.New("settings: must be a JSON object")
	}
	// Parse the settings now: an unknown field or a type error is a 400 at the
	// write boundary, not a boot failure later.
	if _, err := parsePlatformSettings(record.Platform, record.Settings); err != nil {
		return err
	}
	if record.Routes == nil {
		record.Routes = map[string]string{}
	}
	routes := make(map[string]string, len(record.Routes))
	for platformChannel, boardChannel := range record.Routes {
		platformChannel = strings.TrimSpace(platformChannel)
		boardChannel = strings.TrimSpace(boardChannel)
		if platformChannel == "" {
			return errors.New("routes: a platform channel can't be empty")
		}
		if len(platformChannel) > 200 || strings.ContainsRune(platformChannel, 0) {
			return fmt.Errorf("routes: platform channel %q is not usable", platformChannel)
		}
		if err := protocol.ValidateChannel(boardChannel); err != nil {
			return fmt.Errorf("routes.%s: %v", platformChannel, err)
		}
		routes[platformChannel] = boardChannel
	}
	record.Routes = routes
	if record.StartChannels == nil {
		record.StartChannels = []string{}
	}
	channels := make([]string, 0, len(record.StartChannels))
	seen := make(map[string]struct{}, len(record.StartChannels))
	for _, channel := range record.StartChannels {
		channel = strings.TrimSpace(channel)
		if channel == "" || len(channel) > 200 || strings.ContainsRune(channel, 0) {
			return fmt.Errorf("startChannels: %q is not a platform channel", channel)
		}
		if _, duplicate := seen[channel]; duplicate {
			continue
		}
		seen[channel] = struct{}{}
		channels = append(channels, channel)
	}
	if len(channels) == 0 {
		// A route names a channel the entry may start a thread in. Make the
		// design's default explicit: a mapped channel is startable.
		for channel := range routes {
			channels = append(channels, channel)
		}
	}
	record.StartChannels = channels
	return nil
}

// platformSettings is the parsed platform configuration. Each platform has a
// closed schema; the structs are decoded with DisallowUnknownFields so a typo
// never becomes a silent default.
type platformSettings struct {
	slack   slackSettings
	github  githubSettings
	linear  linearSettings
	chatsdk chatsdkSettings
}

type slackSettings struct {
	BotUserID    string `json:"botUserId,omitempty"`
	BotID        string `json:"botId,omitempty"`
	TeamID       string `json:"teamId,omitempty"`
	APIBaseURL   string `json:"apiBaseUrl,omitempty"`
	JoinChannels bool   `json:"joinChannels,omitempty"`
}

type githubSettings struct {
	BotUserID      int64    `json:"botUserId,omitempty"`
	BotLogin       string   `json:"botLogin,omitempty"`
	InstallationID int64    `json:"installationId,omitempty"`
	Repos          []string `json:"repos,omitempty"`
	WriteRepos     []string `json:"writeRepos,omitempty"`
	AllowAllRepos  bool     `json:"allowAllRepos,omitempty"`
	AllowAllWrites bool     `json:"allowAllWrites,omitempty"`
	DetectViewer   bool     `json:"detectViewer,omitempty"`
	APIBaseURL     string   `json:"apiBaseUrl,omitempty"`
}

type linearSettings struct {
	BotUserID     string   `json:"botUserId,omitempty"`
	TeamKeys      []string `json:"teamKeys,omitempty"`
	AllowAllTeams bool     `json:"allowAllTeams,omitempty"`
	TokenScheme   string   `json:"tokenScheme,omitempty"`
	APIBaseURL    string   `json:"apiBaseUrl,omitempty"`
}

type chatsdkSettings struct {
	Platform  string   `json:"platform,omitempty"`
	BridgeURL string   `json:"bridgeUrl,omitempty"`
	SelfIDs   []string `json:"selfIds,omitempty"`
}

func parsePlatformSettings(platform string, raw json.RawMessage) (platformSettings, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return platformSettings{}, errors.New("settings: must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	parsed := platformSettings{}
	switch platform {
	case syncPlatformSlack:
		if err := decoder.Decode(&parsed.slack); err != nil {
			return platformSettings{}, fmt.Errorf("settings: %w", err)
		}
	case syncPlatformGitHub:
		if err := decoder.Decode(&parsed.github); err != nil {
			return platformSettings{}, fmt.Errorf("settings: %w", err)
		}
	case syncPlatformLinear:
		if err := decoder.Decode(&parsed.linear); err != nil {
			return platformSettings{}, fmt.Errorf("settings: %w", err)
		}
	case syncPlatformChatSDK:
		if err := decoder.Decode(&parsed.chatsdk); err != nil {
			return platformSettings{}, fmt.Errorf("settings: %w", err)
		}
		if strings.TrimSpace(parsed.chatsdk.Platform) == "" {
			return platformSettings{}, errors.New("settings.platform: a chatsdk entry needs its sidecar platform name")
		}
		if strings.TrimSpace(parsed.chatsdk.BridgeURL) == "" {
			return platformSettings{}, errors.New("settings.bridgeUrl: a chatsdk entry needs the bridge base URL")
		}
		if len(parsed.chatsdk.SelfIDs) == 0 {
			return platformSettings{}, errors.New("settings.selfIds: chatsdk needs its own author IDs so it doesn't import its own posts")
		}
		if err := validateBridgeURL(parsed.chatsdk.BridgeURL); err != nil {
			return platformSettings{}, err
		}
	default:
		return platformSettings{}, fmt.Errorf("settings: unknown platform %q", platform)
	}
	return parsed, nil
}

func validateBridgeURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("settings.bridgeUrl: %v", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("settings.bridgeUrl: must be http or https")
	}
	if parsed.Host == "" {
		return errors.New("settings.bridgeUrl: needs a host")
	}
	return nil
}

// buildSyncProvider resolves the credentials and constructs the adapter. A
// credential or configuration failure returns a status and an error whose text
// never contains a credential value.
func buildSyncProvider(ctx context.Context, tenantID uuid.UUID, record store.BoardSyncEntryRecord, resolver SecretResolver) (boardsync.Provider, credentialReport, error) {
	report := resolver.resolveEntryCredential(ctx, tenantID, record.Platform, record.SecretRef)
	if report.Status != CredentialConfigured {
		return nil, report, fmt.Errorf("credential %s: %s", report.Status, strings.Join(append(append([]string(nil), report.Missing...), report.Invalid...), ","))
	}
	settings, err := parsePlatformSettings(record.Platform, record.Settings)
	if err != nil {
		return nil, credentialReport{Status: CredentialInvalid}, err
	}
	key := boardsync.Key(record.Key)
	switch record.Platform {
	case syncPlatformSlack:
		provider, err := slack.New(slack.Config{
			Key:           key,
			SigningSecret: report.Secret["signingSecret"],
			BotToken:      report.Secret["botToken"],
			BotUserID:     settings.slack.BotUserID,
			BotID:         settings.slack.BotID,
			TeamID:        settings.slack.TeamID,
			APIBaseURL:    settings.slack.APIBaseURL,
			JoinChannels:  settings.slack.JoinChannels,
		})
		return provider, report, err
	case syncPlatformGitHub:
		provider, err := github.New(github.Config{
			Key:            key,
			WebhookSecret:  report.Secret["webhookSecret"],
			Token:          report.Secret["token"],
			BotUserID:      settings.github.BotUserID,
			BotLogin:       settings.github.BotLogin,
			InstallationID: settings.github.InstallationID,
			DetectViewer:   settings.github.DetectViewer,
			Repos:          settings.github.Repos,
			AllowAllRepos:  settings.github.AllowAllRepos,
			WriteRepos:     settings.github.WriteRepos,
			AllowAllWrites: settings.github.AllowAllWrites,
			APIBaseURL:     settings.github.APIBaseURL,
		})
		return provider, report, err
	case syncPlatformLinear:
		provider, err := linear.New(linear.Config{
			Key:           key,
			WebhookSecret: report.Secret["webhookSecret"],
			APIToken:      report.Secret["apiToken"],
			TokenScheme:   settings.linear.TokenScheme,
			TeamKeys:      settings.linear.TeamKeys,
			AllowAllTeams: settings.linear.AllowAllTeams,
			BotUserID:     settings.linear.BotUserID,
			APIBaseURL:    settings.linear.APIBaseURL,
		})
		return provider, report, err
	case syncPlatformChatSDK:
		client, err := chatsdk.NewClient(chatsdk.ClientConfig{
			BaseURL:  settings.chatsdk.BridgeURL,
			Secret:   report.Secret["bridgeSecret"],
			Key:      key,
			Platform: boardsync.Platform(settings.chatsdk.Platform),
			SelfIDs:  settings.chatsdk.SelfIDs,
		})
		if err != nil {
			return nil, report, err
		}
		caps := boardsync.Capabilities{StartThreads: true, Edits: true, Deletes: true, Reactions: true}
		remote, err := chatsdk.NewRemoteAdapter(client, boardsync.Platform(settings.chatsdk.Platform), caps)
		if err != nil {
			return nil, report, err
		}
		provider, err := chatsdk.New(chatsdk.Config{
			Key: key, Adapter: remote, SelfIDs: settings.chatsdk.SelfIDs, Caps: caps,
		})
		return provider, report, err
	default:
		return nil, credentialReport{Status: CredentialInvalid}, fmt.Errorf("unknown platform %q", record.Platform)
	}
}

// bridgeSecretOf returns the per-entry HMAC secret for a chatsdk connection.
// It is kept beside the provider so the board's bridge event route can verify a
// sidecar request without re-reading the secret store per request.
func bridgeSecretOf(provider boardsync.Provider, report credentialReport) (boardsync.Secret, bool) {
	if provider == nil || provider.Platform() == "" {
		return "", false
	}
	secret, ok := report.Secret["bridgeSecret"]
	if !ok || secret.Empty() {
		return "", false
	}
	return secret, true
}
