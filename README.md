# model-watch

A lightweight Go CLI that polls an OpenAI-compatible `/v1/models` endpoint, saves the result to a local JSON snapshot, and alerts a Discord or Slack webhook when models are added or removed.

## Configuration

All configuration is via environment variables:

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `OPENAI_BASE_URL` | yes | — | Base URL of the OpenAI-compatible API, *without* the `/v1` suffix (e.g. `https://api.openai.com`) |
| `WEBHOOK_URL` | yes | — | Webhook URL for notifications (Discord or Slack incoming webhook) |
| `NOTIFY_TYPE` | no | `discord` | Webhook format: `discord` or `slack` |
| `OPENAI_API_KEY` | no | *(unset)* | API key; omit for keyless servers |
| `SNAPSHOT_PATH` | no | `models-snapshot.json` | Path to the local snapshot file |

## Usage

Example usage with a discord webhook
```bash
export OPENAI_BASE_URL="https://your-api.example.com"
export WEBHOOK_URL="https://discord.com/api/webhooks/..."
export NOTIFY_TYPE="discord"
export OPENAI_API_KEY="sk-..."   # optional

go run .
```

Or build and run the binary:

```bash
go build -o model-watch .
./model-watch
```

## How it works

1. Fetches the model list from `<BASE_URL>/v1/models`.
2. Compares against the previous snapshot (if one exists).
3. If changes are detected, sends webhook message(s):
   - a **green** message listing newly **added** models (when any are added)
   - a **red** message listing **removed** models (when any are removed)

   Discord messages use embeds; Slack messages use coloured attachments.
   A single run therefore sends 0, 1, or 2 messages depending on what changed.
4. Updates the snapshot for the next run.

On the first run, no comparison is made — the baseline snapshot is saved and no webhook is fired.
