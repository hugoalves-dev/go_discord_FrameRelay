package discordbot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/hugoalves-dev/go_discord_FrameRelay/internal/relaycontrol"
)

type Backend interface {
	CreateShare(context.Context, relaycontrol.CreateShareRequest) (relaycontrol.ShareIntent, error)
	CreateWatch(context.Context, string, int) (relaycontrol.WatchLaunch, error)
}

type Interaction struct {
	Subcommand    string
	GuildID       string
	ChannelID     string
	UserID        string
	Code          string
	ReadyIntentID string
}

type LinkButton struct {
	Label string
	URL   string
}

// Announcement is a public follow-up in a DM or Group DM. The bot is usually not a
// member there, so it cannot post the ready message later; recipients use this
// button to request their own watch link once the share is live.
type Announcement struct {
	Content       string
	ReadyIntentID string
}

type Response struct {
	Content      string
	Ephemeral    bool
	Button       *LinkButton
	Announcement *Announcement
}

const privateActivityNotice = "Discord Activities are not available in this conversation, so FrameRelay opens in the desktop viewer."

// isPrivate reports a DM, Group DM, or bot DM. Discord sends no guild there.
func (i Interaction) isPrivate() bool { return i.GuildID == "" }

type CommandHandler struct {
	backend  Backend
	shareTTL time.Duration
	watchTTL time.Duration
}

func NewCommandHandler(backend Backend, shareTTL, watchTTL time.Duration) *CommandHandler {
	return &CommandHandler{backend: backend, shareTTL: shareTTL, watchTTL: watchTTL}
}

// ActivityBackend is optional so older integrations retain desktop links.
type ActivityBackend interface {
	CreateActivity(context.Context, relaycontrol.CreateActivityRequest) (relaycontrol.ActivityIntent, error)
}

type ReadyActivityBackend interface {
	CreateActivityFromReady(context.Context, string, relaycontrol.CreateReadyActivityRequest) (relaycontrol.ActivityIntent, error)
}

type ReadyWatchBackend interface {
	CreateWatchFromReady(context.Context, string, relaycontrol.CreateReadyActivityRequest) (relaycontrol.WatchLaunch, error)
}

func (h *CommandHandler) HandleReadyFallback(ctx context.Context, interaction Interaction) Response {
	backend, ok := h.backend.(ReadyWatchBackend)
	if !ok || interaction.ReadyIntentID == "" || interaction.ChannelID == "" {
		return Response{Content: "Run `/framerelay watch code:<host-code>` for a personal desktop watch link.", Ephemeral: true}
	}
	watch, err := backend.CreateWatchFromReady(ctx, interaction.ReadyIntentID, relaycontrol.CreateReadyActivityRequest{
		GuildID: interaction.GuildID, ChannelID: interaction.ChannelID,
		RequestedByUserID: interaction.UserID, TTLSeconds: int(h.watchTTL.Seconds()),
	})
	if err != nil || watch.LaunchURL == "" {
		return Response{Content: "This share is not live yet or has ended. Try again once the host is sharing, or run `/framerelay watch code:<host-code>`.", Ephemeral: true}
	}
	content := "Discord viewing is unavailable. Open your personal FrameRelay desktop link instead."
	if interaction.isPrivate() {
		content = privateActivityNotice + " Open your personal link instead."
	}
	return Response{Content: content, Ephemeral: true,
		Button: &LinkButton{Label: "Watch in FrameRelay", URL: watch.LaunchURL}}
}

// InitialResponse creates the intent before callback 12. When setup is unavailable,
// it defers instead; the caller then runs Handle for the existing desktop link.
func (h *CommandHandler) InitialResponse(ctx context.Context, interaction Interaction, respond func(context.Context, bool) error) (bool, error) {
	// Activities are only offered in guilds: the RelayControl Activity flow is guild-bound.
	if (interaction.Subcommand == "watch" || interaction.ReadyIntentID != "") && !interaction.isPrivate() && interaction.ChannelID != "" {
		_, codeSupported := h.backend.(ActivityBackend)
		_, readySupported := h.backend.(ReadyActivityBackend)
		if (interaction.ReadyIntentID != "" && readySupported) || (interaction.ReadyIntentID == "" && codeSupported) {
			budget := 1500 * time.Millisecond
			const callbackReserve = 500 * time.Millisecond
			if deadline, ok := ctx.Deadline(); ok {
				remaining := time.Until(deadline) - callbackReserve
				if remaining < budget {
					budget = remaining
				}
			}
			if budget <= 0 {
				return false, respond(ctx, false)
			}
			attempt, cancel := context.WithTimeout(ctx, budget)
			var intent relaycontrol.ActivityIntent
			var err error
			if interaction.ReadyIntentID != "" {
				intent, err = h.backend.(ReadyActivityBackend).CreateActivityFromReady(attempt, interaction.ReadyIntentID, relaycontrol.CreateReadyActivityRequest{
					GuildID: interaction.GuildID, ChannelID: interaction.ChannelID,
					RequestedByUserID: interaction.UserID, TTLSeconds: 120,
				})
			} else {
				intent, err = h.backend.(ActivityBackend).CreateActivity(attempt, relaycontrol.CreateActivityRequest{
					Code: interaction.Code, GuildID: interaction.GuildID, ChannelID: interaction.ChannelID,
					RequestedByUserID: interaction.UserID, TTLSeconds: int(h.watchTTL.Seconds()),
				})
			}
			cancel()
			if err == nil && intent.ID != "" && intent.ExpiresAt.After(time.Now()) && ctx.Err() == nil {
				// An unsuccessful callback may have reached Discord. Do not double acknowledge.
				err := respond(ctx, true)
				var rejected *rest.Error
				// A definite invalid callback (e.g. Activities not enabled) was not acknowledged.
				// Transport failures and Discord's already-acknowledged code are ambiguous.
				if errors.As(err, &rejected) && rejected.Response != nil && rejected.Response.StatusCode == http.StatusBadRequest && rejected.Code != 40060 && ctx.Err() == nil {
					return false, respond(ctx, false)
				}
				return true, err
			}
		}
	}
	return false, respond(ctx, false)
}

func (h *CommandHandler) Handle(ctx context.Context, interaction Interaction) Response {
	if interaction.ChannelID == "" {
		return Response{Content: "This command needs a Discord channel.", Ephemeral: true}
	}
	switch interaction.Subcommand {
	case "share":
		intent, err := h.backend.CreateShare(ctx, relaycontrol.CreateShareRequest{
			Provider: "discord", GuildID: interaction.GuildID, ChannelID: interaction.ChannelID,
			RequestedByUserID: interaction.UserID, TTLSeconds: int(h.shareTTL.Seconds()),
		})
		if err != nil {
			return Response{Content: userError(err), Ephemeral: true}
		}
		response := Response{
			Content: fmt.Sprintf("Ready to start FrameRelay.\nThis launch link expires at %s.",
				intent.ExpiresAt.UTC().Format(time.RFC3339)),
			Ephemeral: true,
			Button:    &LinkButton{Label: "Open FrameRelay and share", URL: intent.LaunchURL},
		}
		if interaction.isPrivate() {
			response.Content += "\nPeople in this conversation can select **Watch in FrameRelay** on the announcement once you are sharing."
			response.Announcement = &Announcement{
				Content: fmt.Sprintf("<@%s> is starting a FrameRelay screen share. Once it is live, select **Watch in FrameRelay** for your personal desktop link. %s",
					interaction.UserID, privateActivityNotice),
				ReadyIntentID: intent.ID,
			}
		}
		return response
	case "watch":
		watch, err := h.backend.CreateWatch(ctx, interaction.Code, int(h.watchTTL.Seconds()))
		if err != nil {
			return Response{Content: userError(err), Ephemeral: true}
		}
		content := fmt.Sprintf("Open this FrameRelay share before %s.", watch.ExpiresAt.UTC().Format(time.RFC3339))
		if interaction.isPrivate() {
			content = privateActivityNotice + " " + content
		}
		return Response{
			Content:   content,
			Ephemeral: true,
			Button:    &LinkButton{Label: "Watch in FrameRelay", URL: watch.LaunchURL},
		}
	default:
		return Response{Content: "Unknown FrameRelay command.", Ephemeral: true}
	}
}

func userError(err error) string {
	var requestError *relaycontrol.RequestError
	if errors.As(err, &requestError) {
		switch requestError.Code {
		case "invalid_code":
			return "That code is not valid, or the share has ended."
		case "session_full":
			return "That FrameRelay share is full."
		case "session_unavailable":
			return "That FrameRelay share has ended or is unavailable."
		case "invalid_ttl":
			return "The FrameRelay launch link could not be created. Try again."
		}
	}
	return "FrameRelay is temporarily unavailable. Try again shortly."
}

func CommandDefinitions() []discord.ApplicationCommandCreate {
	return []discord.ApplicationCommandCreate{
		discord.SlashCommandCreate{
			Name:        "framerelay",
			Description: "Start or watch a FrameRelay screen share",
			IntegrationTypes: []discord.ApplicationIntegrationType{
				discord.ApplicationIntegrationTypeGuildInstall,
				discord.ApplicationIntegrationTypeUserInstall,
			},
			Contexts: []discord.InteractionContextType{
				discord.InteractionContextTypeGuild,
				discord.InteractionContextTypeBotDM,
				discord.InteractionContextTypePrivateChannel,
			},
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionSubCommand{Name: "share", Description: "Start a screen share from FrameRelay"},
				discord.ApplicationCommandOptionSubCommand{
					Name: "watch", Description: "Watch a FrameRelay share inside Discord",
					Options: []discord.ApplicationCommandOption{
						discord.ApplicationCommandOptionString{Name: "code", Description: "The six-character FrameRelay share code", Required: true, MinLength: intPointer(6), MaxLength: intPointer(12)},
					},
				},
			},
		},
	}
}

func intPointer(value int) *int { return &value }
