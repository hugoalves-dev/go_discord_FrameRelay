package discordbot

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/hugoalves-dev/go_discord_FrameRelay/internal/relaycontrol"
)

const privateIntentID = "123e4567-e89b-12d3-a456-426614174000"

func TestShareInPrivateConversationAnnouncesWatchButtonWithoutCapability(t *testing.T) {
	backend := &fakeBackend{share: relaycontrol.ShareIntent{
		ID: privateIntentID, LaunchURL: "https://framerelay.example/open/share/secret", ExpiresAt: time.Now().Add(10 * time.Minute),
	}}
	response := NewCommandHandler(backend, 10*time.Minute, 30*time.Minute).Handle(context.Background(), Interaction{
		Subcommand: "share", ChannelID: "dm-1", UserID: "host",
	})
	if backend.shareRequest.GuildID != "" || backend.shareRequest.ChannelID != "dm-1" || backend.shareRequest.RequestedByUserID != "host" {
		t.Fatalf("unexpected RelayControl request: %#v", backend.shareRequest)
	}
	if !response.Ephemeral || response.Button == nil || response.Button.URL != backend.share.LaunchURL {
		t.Fatalf("host must privately receive the launch link: %#v", response)
	}
	if response.Announcement == nil || response.Announcement.ReadyIntentID != privateIntentID {
		t.Fatalf("private share must announce a watch button: %#v", response.Announcement)
	}
	if strings.Contains(response.Announcement.Content, "https://") {
		t.Fatalf("announcement must not leak the host launch capability: %q", response.Announcement.Content)
	}
}

func TestShareInGuildKeepsReadyPublicationFlow(t *testing.T) {
	backend := &fakeBackend{share: relaycontrol.ShareIntent{ID: privateIntentID, LaunchURL: "https://x/open/share/s", ExpiresAt: time.Now().Add(time.Minute)}}
	response := NewCommandHandler(backend, time.Minute, time.Minute).Handle(context.Background(), Interaction{
		Subcommand: "share", GuildID: "guild", ChannelID: "voice", UserID: "host",
	})
	if response.Announcement != nil {
		t.Fatalf("guild shares are announced by the ready watcher: %#v", response.Announcement)
	}
}

func TestPrivateConversationNeverLaunchesActivity(t *testing.T) {
	backend := &activityBackend{activity: relaycontrol.ActivityIntent{ID: "intent", ExpiresAt: time.Now().Add(time.Minute)}}
	handler := NewCommandHandler(backend, time.Minute, time.Minute)
	for _, interaction := range []Interaction{
		{Subcommand: "watch", Code: "ABC123", ChannelID: "dm", UserID: "viewer"},
		{ReadyIntentID: privateIntentID, ChannelID: "dm", UserID: "viewer"},
	} {
		launched, err := handler.InitialResponse(context.Background(), interaction, func(_ context.Context, launch bool) error {
			if launch {
				t.Fatal("must not launch an Activity in a private conversation")
			}
			return nil
		})
		if launched || err != nil || backend.request.Code != "" || backend.readyID != "" {
			t.Fatalf("launched=%v err=%v backend=%#v", launched, err, backend)
		}
	}
}

func TestPrivateReadyButtonCreatesPersonalDesktopLinkForThatChannel(t *testing.T) {
	backend := &activityBackend{watchReady: relaycontrol.WatchLaunch{LaunchURL: "https://relay.example/open/watch/personal", ExpiresAt: time.Now().Add(time.Minute)}}
	response := NewCommandHandler(backend, time.Minute, 2*time.Minute).HandleReadyFallback(context.Background(),
		Interaction{ReadyIntentID: privateIntentID, ChannelID: "gdm", UserID: "viewer"})
	if backend.watchReadyID != privateIntentID || backend.watchReadyRequest.GuildID != "" ||
		backend.watchReadyRequest.ChannelID != "gdm" || backend.watchReadyRequest.RequestedByUserID != "viewer" {
		t.Fatalf("unexpected ready watch request: %#v", backend.watchReadyRequest)
	}
	if !response.Ephemeral || response.Button == nil || response.Button.URL != backend.watchReady.LaunchURL ||
		!strings.Contains(response.Content, "Activities are not available") {
		t.Fatalf("unexpected fallback: %#v", response)
	}
}

func TestPrivateWatchCommandExplainsDesktopFallback(t *testing.T) {
	backend := &fakeBackend{watch: relaycontrol.WatchLaunch{LaunchURL: "https://relay.example/open/watch/p", ExpiresAt: time.Now().Add(time.Minute)}}
	response := NewCommandHandler(backend, time.Minute, time.Minute).Handle(context.Background(),
		Interaction{Subcommand: "watch", ChannelID: "dm", UserID: "viewer", Code: "ABC123"})
	if !response.Ephemeral || response.Button == nil || !strings.Contains(response.Content, "Activities are not available") {
		t.Fatalf("unexpected private watch response: %#v", response)
	}
}

func TestPublishReadySkipsPrivateConversations(t *testing.T) {
	adapter := &Adapter{} // nil client: a private intent must not post a bot message.
	messageID, err := adapter.PublishReady(context.Background(), relaycontrol.PendingIntent{ID: privateIntentID, ChannelID: "123"}, relaycontrol.IntentStatus{})
	if err != nil || messageID != "" {
		t.Fatalf("messageID=%q err=%v", messageID, err)
	}
}

func TestPrivateAnnouncementMessageHasButtonAndNoMentions(t *testing.T) {
	message := privateAnnouncementMessage(Announcement{Content: "ready soon", ReadyIntentID: privateIntentID})
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "framerelay:watch:"+privateIntentID) || strings.Contains(string(encoded), "https://") {
		t.Fatalf("unexpected announcement: %s", encoded)
	}
	if message.AllowedMentions == nil || len(message.AllowedMentions.Parse) != 0 {
		t.Fatalf("announcement must not ping anyone: %s", encoded)
	}
	if message.Flags.Has(discord.MessageFlagEphemeral) {
		t.Fatal("announcement must be visible to conversation participants")
	}
}

func TestCommandIsUserInstallableInPrivateChannels(t *testing.T) {
	command := CommandDefinitions()[0].(discord.SlashCommandCreate)
	if !slices.Contains(command.IntegrationTypes, discord.ApplicationIntegrationTypeUserInstall) ||
		!slices.Contains(command.Contexts, discord.InteractionContextTypePrivateChannel) ||
		!slices.Contains(command.Contexts, discord.InteractionContextTypeBotDM) ||
		!slices.Contains(command.Contexts, discord.InteractionContextTypeGuild) {
		t.Fatalf("unexpected command reach: %#v %#v", command.IntegrationTypes, command.Contexts)
	}
}
