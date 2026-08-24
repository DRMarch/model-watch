// Command model-watch polls an OpenAI-compatible /v1/models endpoint,
// keeps a local snapshot of the models seen, and notifies a Discord or
// Slack webhook when models are added or removed.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	colourAdded   = 0x2ECC71
	colourRemoved = 0xE74C3C
)

// Supported webhook notification providers.
const (
	notificationDiscord = "discord"
	notificationSlack   = "slack"
)

// --- types ---

// Model is a single model listed by the /v1/models endpoint.
type Model struct {
	ID      string `json:"id"`       // Unique model identifier.
	Object  string `json:"object"`   // Object type, typically "model".
	Created int64  `json:"created"`  // Unix timestamp of when the model was created.
	OwnedBy string `json:"owned_by"` // Organisation that owns the model.
}

// ModelsResponse is the JSON response returned by the /v1/models endpoint.
type ModelsResponse struct {
	Object string  `json:"object"` // Object type, typically "list".
	Data   []Model `json:"data"`   // The models returned by the endpoint.
}

// Snapshot is a saved record of the models seen on a previous run.
type Snapshot struct {
	LastRun time.Time `json:"last_run"` // Time the snapshot was saved.
	Models  []Model   `json:"models"`   // Models seen at that time.
}

// Diff holds the models added since, and removed from, the previous snapshot.
type Diff struct {
	Added   []Model // Models present now but not in the previous snapshot.
	Removed []Model // Models in the previous snapshot but gone now.
}

// HasChanges reports whether the diff contains any added or removed models.
func (d Diff) HasChanges() bool {
	return len(d.Added) > 0 || len(d.Removed) > 0
}

// discordField is a Discord embed field.
type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

// discordEmbed is a Discord embed object.
type discordEmbed struct {
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Colour      int            `json:"color"`
	Fields      []discordField `json:"fields"`
	Timestamp   string         `json:"timestamp"`
}

// webhookPayload is the top-level Discord webhook request body.
type webhookPayload struct {
	Embeds []discordEmbed `json:"embeds"`
}

// slackField is a Slack attachment field.
type slackField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

// slackAttachment is a coloured Slack attachment.
type slackAttachment struct {
	Colour string       `json:"color"`
	Title  string       `json:"title"`
	Fields []slackField `json:"fields"`
}

// slackPayload is the top-level Slack incoming webhook request body.
type slackPayload struct {
	Text        string            `json:"text"`
	Attachments []slackAttachment `json:"attachments"`
}

// Config holds the runtime configuration read from environment variables.
type Config struct {
	BaseURL      string // OpenAI-compatible API base URL, without the /v1 suffix.
	APIKey       string // Optional API key sent as a Bearer token.
	WebhookURL   string // Webhook URL for notifications (Discord or Slack).
	SnapshotPath string // Path to the JSON snapshot file.
	NotifyType   string // Webhook format: "discord" or "slack".
}

// --- entry point ---

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := &http.Client{Timeout: 30 * time.Second}

	if err := run(ctx, client); err != nil {
		slog.Error("model-watch failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, client *http.Client) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	prev, err := loadSnapshot(cfg.SnapshotPath)
	if err != nil {
		return fmt.Errorf("loading snapshot: %w", err)
	}

	models, err := fetchModels(ctx, client, cfg)
	if err != nil {
		return fmt.Errorf("fetching models: %w", err)
	}

	// If there is no previous snapshot, save the current models as the
	// baseline and return without notifying.
	if prev == nil {
		slog.Info("no previous snapshot; saving baseline", "path", cfg.SnapshotPath)
		if err := saveSnapshot(cfg.SnapshotPath, models); err != nil {
			return fmt.Errorf("saving snapshot: %w", err)
		}
		return nil
	}

	diff := diffModels(prev, models)
	if !diff.HasChanges() {
		slog.Info("no changes detected")
		return nil
	}

	slog.Info("changes detected", "added", len(diff.Added), "removed", len(diff.Removed))

	if err := sendWebhook(ctx, client, cfg.NotifyType, cfg.WebhookURL, diff); err != nil {
		return fmt.Errorf("sending webhook: %w", err)
	}

	if err := saveSnapshot(cfg.SnapshotPath, models); err != nil {
		return fmt.Errorf("saving snapshot: %w", err)
	}

	slog.Info("snapshot updated, notification sent")
	return nil
}

// --- config ---

func loadConfig() (Config, error) {
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		return Config{}, fmt.Errorf("OPENAI_BASE_URL is required")
	}

	webhookURL := os.Getenv("WEBHOOK_URL")
	if webhookURL == "" {
		return Config{}, fmt.Errorf("WEBHOOK_URL is required")
	}

	notifyType := getEnvOrDefault("NOTIFY_TYPE", notificationDiscord)
	if notifyType != notificationDiscord && notifyType != notificationSlack {
		return Config{}, fmt.Errorf("NOTIFY_TYPE must be %q or %q, got %q", notificationDiscord, notificationSlack, notifyType)
	}

	return Config{
		BaseURL:      baseURL,
		APIKey:       os.Getenv("OPENAI_API_KEY"),
		WebhookURL:   webhookURL,
		SnapshotPath: getEnvOrDefault("SNAPSHOT_PATH", "models-snapshot.json"),
		NotifyType:   notifyType,
	}, nil
}

func getEnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// --- watcher ---

func fetchModels(ctx context.Context, client *http.Client, cfg Config) ([]Model, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing base URL: %w", err)
	}
	u.Path = path.Join(u.Path, "v1/models")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("performing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("reading error body: %w", err)
		}
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var result ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	_, _ = io.Copy(io.Discard, resp.Body) // drain for connection reuse
	return result.Data, nil
}

func loadSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

func saveSnapshot(path string, models []Model) error {
	snap := Snapshot{
		LastRun: time.Now().UTC(),
		Models:  models,
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating snapshot directory: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

func diffModels(prev *Snapshot, current []Model) Diff {
	if prev == nil {
		return Diff{}
	}

	prevIDs := make(map[string]struct{}, len(prev.Models))
	for _, m := range prev.Models {
		prevIDs[m.ID] = struct{}{}
	}

	currIDs := make(map[string]struct{}, len(current))
	for _, m := range current {
		currIDs[m.ID] = struct{}{}
	}

	var added []Model
	for _, m := range current {
		if _, ok := prevIDs[m.ID]; !ok {
			added = append(added, m)
		}
	}

	var removed []Model
	for _, m := range prev.Models {
		if _, ok := currIDs[m.ID]; !ok {
			removed = append(removed, m)
		}
	}

	return Diff{Added: added, Removed: removed}
}

// --- notifications ---

// payloadBuilder marshals a notification for a list of models into the JSON
// format expected by a webhook provider.
type payloadBuilder func(models []Model, colour int, label string) ([]byte, error)

// sendWebhook posts up to two messages: a green one for newly added models
// and a red one for removed models. When only one kind of change exists,
// only that message is sent. The provider selects the message format and
// must be "discord" or "slack".
func sendWebhook(ctx context.Context, client *http.Client, provider, webhookURL string, diff Diff) error {
	var build payloadBuilder
	switch provider {
	case notificationSlack:
		build = buildSlackPayload
	default:
		build = buildDiscordPayload
	}

	if len(diff.Added) > 0 {
		if err := sendProviderMessage(ctx, client, webhookURL, build, diff.Added, colourAdded, "Added"); err != nil {
			return fmt.Errorf("sending added models: %w", err)
		}
	}
	if len(diff.Removed) > 0 {
		if err := sendProviderMessage(ctx, client, webhookURL, build, diff.Removed, colourRemoved, "Removed"); err != nil {
			return fmt.Errorf("sending removed models: %w", err)
		}
	}
	return nil
}

func sendProviderMessage(ctx context.Context, client *http.Client, webhookURL string, build payloadBuilder, models []Model, colour int, label string) error {
	data, err := build(models, colour, label)
	if err != nil {
		return fmt.Errorf("marshalling payload: %w", err)
	}
	return sendMessage(ctx, client, webhookURL, data)
}

// sendMessage POSTs a marshalled payload to a webhook URL.
func sendMessage(ctx context.Context, client *http.Client, webhookURL string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("building webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("posting webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("reading webhook error body: %w", err)
		}
		return fmt.Errorf("webhook returned status %d: %s", resp.StatusCode, body)
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// buildDiscordPayload marshals a Discord embed listing the given models.
func buildDiscordPayload(models []Model, colour int, label string) ([]byte, error) {
	embed := discordEmbed{
		Title:       "Model Watcher",
		Description: fmt.Sprintf("%s models", label),
		Colour:      colour,
		Fields: []discordField{
			{
				Name:   fmt.Sprintf("%s (%d)", label, len(models)),
				Value:  formatModelList(models),
				Inline: false,
			},
		},
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	return json.Marshal(webhookPayload{Embeds: []discordEmbed{embed}})
}

// buildSlackPayload marshals a Slack message with an attachment listing the
// given models. The Discord colour integer is converted to a hex string.
func buildSlackPayload(models []Model, colour int, label string) ([]byte, error) {
	attachment := slackAttachment{
		Colour: fmt.Sprintf("#%06X", colour),
		Title:  "Model Watcher",
		Fields: []slackField{
			{
				Title: fmt.Sprintf("%s (%d)", label, len(models)),
				Value: formatModelList(models),
				Short: false,
			},
		},
	}
	return json.Marshal(slackPayload{
		Text:        fmt.Sprintf("%s models", label),
		Attachments: []slackAttachment{attachment},
	})
}

func formatModelList(models []Model) string {
	var sb strings.Builder
	for i, m := range models {
		if i > 0 {
			sb.WriteByte('\n')
		}
		fmt.Fprintf(&sb, "`%s`", m.ID)
		if m.OwnedBy != "" {
			fmt.Fprintf(&sb, " — %s", m.OwnedBy)
		}
	}
	return sb.String()
}
