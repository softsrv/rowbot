//go:build integration

package app_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/softsrv/rowbot/internal/app"
	"github.com/softsrv/rowbot/internal/db"
)

func TestIntegrationRemoveChannelSettingsDeletesGuildSettings(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	q := db.New(pool)
	svc := app.NewDiscordService(q, "", nil)

	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}
	guildID := "guild-remove-channel-settings"
	_, err = q.UpsertGuildSettings(ctx, db.UpsertGuildSettingsParams{
		ID:              id,
		GuildID:         guildID,
		ReportChannelID: "channel-1",
		ChannelName:     "training",
		SetByUserID:     "discord-user-1",
	})
	if err != nil {
		t.Fatalf("UpsertGuildSettings: %v", err)
	}
	t.Cleanup(func() {
		_ = q.DeleteGuildSettings(context.Background(), guildID)
	})

	if err := svc.RemoveChannelSettings(ctx, guildID); err != nil {
		t.Fatalf("RemoveChannelSettings: %v", err)
	}
	if _, configured, err := svc.GetChannelSettings(ctx, guildID); err != nil {
		t.Fatalf("GetChannelSettings: %v", err)
	} else if configured {
		t.Fatal("GetChannelSettings configured = true, want false after removal")
	}
}
