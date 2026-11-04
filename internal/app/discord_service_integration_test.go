//go:build integration

package app_test

import (
	"context"
	"testing"

	"github.com/softsrv/rowbot/internal/app"
	"github.com/softsrv/rowbot/internal/db"
)

func TestIntegrationRemoveChannelSettingsDeletesGuildSettings(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	q := db.New(pool)
	svc := app.NewDiscordService(q, "", nil)

	guildID := seedGuildSettings(t, pool, "guild-remove-channel-settings-"+randSuffix(t)).GuildID

	if err := svc.RemoveChannelSettings(ctx, guildID); err != nil {
		t.Fatalf("RemoveChannelSettings: %v", err)
	}
	if _, configured, err := svc.GetChannelSettings(ctx, guildID); err != nil {
		t.Fatalf("GetChannelSettings: %v", err)
	} else if configured {
		t.Fatal("GetChannelSettings configured = true, want false after removal")
	}
}
