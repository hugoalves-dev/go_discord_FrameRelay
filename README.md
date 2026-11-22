# FrameRelay Discord bot

This standalone Go service connects Discord slash commands to RelayControl launch intents. The desktop client owns screen capture and streaming. The bot launches a Discord Activity viewer or creates personal desktop launch links and publishes a readiness announcement when a share is ready.

## Commands

- `/framerelay share` responds privately with a one-click button that opens the FrameRelay desktop app. It works in server channels, DMs, and Group DMs. The owner starts capture and streaming in that app.
- `/framerelay watch code:<code>` creates a session/context-bound intent and responds with Discord `LAUNCH_ACTIVITY` (callback 12) before any defer. Run it in the voice channel's chat where the Activity will launch. If the Activity API is unavailable or Discord definitively rejects the launch callback, it defers and returns the existing private desktop watch button.

### DMs and Group DMs

FrameRelay works in a 1:1 DM or Group DM with other people, without a shared server:

1. Enable **User Install** under **Installation** in the Discord Developer Portal, with the `applications.commands` scope for the user install type. Each person authorizes FrameRelay on their own account through the install link. Discord shows a user-installed command only to people who installed the app, so a participant needs it installed to run `/framerelay watch`. Anyone in the conversation can still select the announcement button.
2. Register the command globally by leaving `DISCORD_GUILD_ID` unset. A development guild registration does not reach DMs.
3. In the conversation, the host runs `/framerelay share`. The host privately gets the desktop launch button. The conversation gets a public follow-up with a **Watch in FrameRelay** button that contains no link or credential and pings no one.
4. Once the host is sharing, each participant selects **Watch in FrameRelay**. RelayControl checks that the share is live and bound to that conversation, then sends that participant a private desktop watch link. Before the share is live, the button says to try again later. Participants who have the app can also run `/framerelay watch code:<host-code>`.

The bot is usually not a member of private conversations, so it never posts the ready message there; the interaction follow-up replaces it. Discord Activities are guild-only in this bot, so DMs and Group DMs always use the FrameRelay desktop viewer, and the replies say so. Bot DMs follow the same flow.

### Recovery

The bot also recovers ready-but-unpublished shares after restart. It polls status with `watchTtlSeconds=0`, caches readiness without minting a watch capability, and uses a stable Discord message nonce so a retry after a successful post does not duplicate the announcement. The public announcement includes **Assistir no Discord**. Each click asks RelayControl for an Activity intent bound to that participant, guild, and channel. If the launch is unavailable, the bot replies privately with a fresh personal desktop watch link. The announcement contains no shared watch credential. `/framerelay watch` remains available with the host's code.
The bot also recovers ready-but-unpublished shares after restart. It checks status with `watchTtlSeconds=0`, caches readiness without minting a watch capability, and uses a stable Discord message nonce so a retry after a successful post does not duplicate the announcement. The public announcement includes **Assistir no Discord**. Each click asks RelayControl for an Activity intent bound to that participant, guild, and channel. If the launch is unavailable, the bot replies privately with a fresh personal desktop watch link. The announcement contains no shared watch credential. `/framerelay watch` remains available with the host's code.

## Launch events

The bot learns about ready shares from RelayControl's authenticated server-sent event stream, `GET /api/launch-intents/events`, instead of polling. On every connection it waits for the `connected` frame, then reconciles `GET /api/launch-intents/pending` once. Shares that became ready while it was disconnected or restarting are published then. Each `session_ready` event (`{"intentId"}` only) triggers the same reconciliation, which publishes through the existing Discord flow and acknowledges with `POST /api/launch-intents/{id}/published`.

- Duplicate events are harmless: acknowledged intents leave `/pending`, and the stable message nonce deduplicates a retried post. Delivery is at-least-once. If the bot crashes after posting but before the acknowledgement, the next reconciliation retries the post and Discord's nonce usually, but not always, suppresses the duplicate.
- Failed publications (Discord or API unavailable) are retried with exponential backoff while the stream stays up.
- Disconnects, EOF, idle streams (no frame for 75s; the API sends a heartbeat every 20s) and network errors reconnect with exponential backoff plus jitter, from 1s up to 1m. A `401`/`403` is logged as a credential problem and retried only every 5 minutes.
- Only a `404` (a RelayControl version without the stream) switches to low-frequency fallback. The bot then reconciles and re-probes every `INTENT_FALLBACK_INTERVAL` (default `1m`). Transient disconnects never resume fast polling.
- `/readyz` reports RelayControl ready while the stream is connected (or the API answered `404` in fallback). Discord readiness reads local gateway state. Health checks make no extra API requests.
- Events are parsed with `net/http` and a built-in `text/event-stream` parser. Lines and events are capped at 16 KiB, and unknown event types are ignored. Tokens and watch URLs are never logged.

**Rollout:** deploy RelayControl with the event stream (dotnet_RelayControl#64) first, then this bot. Deploying the bot first is safe because it uses the 404 fallback. Afterwards, check Cloudflare Analytics or request logs: steady-state `/api/launch-intents/pending` traffic should fall from the ~86k/day baseline to a few requests per connection or event. `INTENT_POLL_INTERVAL` is no longer read and can be removed from deployments.

## Configuration

Copy `.env.example` and provide:

- `DISCORD_TOKEN`: bot token; keep it in the deployment secret store.
- `DISCORD_APPLICATION_ID`: Discord application ID.
- `DISCORD_GUILD_ID`: optional development server ID. When omitted, the command is registered globally.
- `RELAYCONTROL_BASE_URL`: RelayControl API base URL. HTTPS is required except for loopback development URLs.
- `RELAYCONTROL_SERVICE_TOKEN`: credential configured for the restricted `launch-intents:bot` policy in RelayControl. Give it only to this service.

The optional `SHARE_INTENT_TTL`, `WATCH_INTENT_TTL`, `INTENT_FALLBACK_INTERVAL` (15s–1h), `HTTP_ADDR`, and `LOG_LEVEL` settings have validated defaults in `.env.example`. The API's public landing URL is configured as `RelayLaunch:PublicBaseUrl`; that endpoint opens the desktop protocol handler and offers the app download page. Set `RelayLaunch:ServiceToken` to the bot's `RELAYCONTROL_SERVICE_TOKEN`.

The Activity intent API attempt is capped at 1.5 seconds and shortened to reserve 500 milliseconds for the initial callback when gateway delivery consumed time. If that reserve is all that remains, the bot immediately defers for the desktop fallback. Initial Discord responses use the interaction creation timestamp plus 2.8 seconds, leaving margin inside Discord's 3-second deadline. An ambiguous network failure after sending callback 12 cannot safely be acknowledged again; retry the command in that case. No Activity success is reported without a valid unexpired intent. See [Activity hosting and setup](activity/README.md).

The bot requests no privileged Discord Gateway intents. Install it in a server with the `bot` and `applications.commands` scopes, or authorize it for personal use with the `applications.commands` scope. In servers, grant it permission to view and send messages in channels where `/framerelay share` is used.

## Run locally

```powershell
Copy-Item .env.example .env
# Fill .env, then load it into the process environment using your preferred secret loader.
$env:GOTOOLCHAIN = 'go1.27.1'
go run ./cmd/framerelay-bot
```

For a development server, set `DISCORD_GUILD_ID` so Discord updates the slash command immediately. Without it, the bot replaces its global `/framerelay` command.

## Docker

```sh
docker build -t framerelay-discord-bot .
docker run --rm --env-file .env -p 8080:8080 framerelay-discord-bot
```

The container runs as an unprivileged user. `/healthz` reports process health; `/readyz` becomes healthy only after the Discord Gateway is connected and RelayControl responds. Do not expose the health port publicly unless needed by the orchestrator.

## Deploy to a VPS

The `CI` workflow runs formatting, vet, race-enabled tests, and a Go build on pull requests. A push to `main` publishes `ghcr.io/hugoalves-dev/framerelay-discord-bot` with a commit tag and `latest`, then deploys it to the VPS over SSH. Manual workflow runs default to `deploy: false`; selecting `true` publishes and deploys that commit.

Before the first deploy:

1. Install Docker Engine and the Docker Compose plugin on the VPS, and allow the SSH user to run Docker.
2. Ensure the shared external Docker network `infra_network` exists on the VPS.
3. Create `/docker/frameRelayDiscordBot/.env` from the repository's `.env.example`. Set `DISCORD_TOKEN`, `DISCORD_APPLICATION_ID`, `RELAYCONTROL_BASE_URL`, and `RELAYCONTROL_SERVICE_TOKEN`; keep this file only on the VPS.
4. Add these repository Actions secrets: `VPS_HOST`, `VPS_USER`, and `VPS_SSH_KEY`. `VPS_PORT` is optional and defaults to `22`. `VPS_APP_DIR` is optional and defaults to `/docker/frameRelayDiscordBot`; use this bot-specific directory rather than the RelayControl application directory.

The deploy job uses its short-lived `GITHUB_TOKEN` to pull the exact commit-tagged GHCR image and removes the temporary Docker credentials when it finishes. The Compose file defaults to `latest` for manual deployments. Discord and RelayControl credentials stay in the VPS `.env` and are not sent to GitHub Actions.

## Checks

```sh
gofmt -w .
go vet ./...
go test -race ./...
go build ./cmd/framerelay-bot
```

No request body, bot token, service credential, deep link, or watch capability is included in application logs. Operational logs use JSON and report only operation names.
