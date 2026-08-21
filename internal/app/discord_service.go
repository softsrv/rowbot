package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/softsrv/rowbot/internal/db"
	"github.com/softsrv/rowbot/internal/discord"
)

// ErrMissingGuildChannel is returned by SetChannel when any required identifier
// (guild ID, channel ID, or user ID) is empty.
var ErrMissingGuildChannel = errors.New("guild_id, channel_id, and set_by_user_id are required")

// DiscordService handles business logic for Discord bot interactions.
// It is independent of the OAuth flow: a Discord user who runs a slash
// command need not have a site account.
type DiscordService struct {
	q          *db.Queries
	botToken   string
	httpClient *http.Client
}

// NewDiscordService constructs a DiscordService.
func NewDiscordService(q *db.Queries, botToken string, httpClient *http.Client) *DiscordService {
	return &DiscordService{q: q, botToken: botToken, httpClient: httpClient}
}

// ListGuildTextChannels returns the guild's plain text channels from Discord.
func (s *DiscordService) ListGuildTextChannels(ctx context.Context, guildID string) ([]discord.Channel, error) {
	return discord.GetGuildChannels(ctx, s.httpClient, s.botToken, guildID)
}

// RegisterFromInteraction records (or updates) a Discord user's registration
// from a slash command. The record is keyed on (discordUserID, guildID), so
// the same user registering from two different servers produces two rows, and
// a repeated /register in the same server is an idempotent upsert that refreshes
// the display name and guild name.
func (s *DiscordService) RegisterFromInteraction(ctx context.Context, discordUserID, discordUsername, guildID, guildName string) (db.DiscordRegistration, error) {
	if err := s.RecordGuildSeen(ctx, guildID, guildName); err != nil {
		slog.WarnContext(ctx, "discord_service: record guild seen failed, proceeding with registration",
			"guild_id", guildID,
			"error", err,
		)
	}

	// Look up whether this Discord user has already linked a site account via
	// OAuth. We fail open: a DB error here must not prevent the registration.
	var linkedUserID pgtype.UUID
	identity, err := s.q.GetOAuthIdentityByProviderUserID(ctx, db.GetOAuthIdentityByProviderUserIDParams{
		Provider:       "discord",
		ProviderUserID: discordUserID,
	})
	switch {
	case err == nil:
		// Identity found — prefer the authoritative stored username.
		if identity.ProviderUsername.Valid {
			discordUsername = identity.ProviderUsername.String
		}
		linkedUserID = pgtype.UUID{Bytes: identity.UserID, Valid: true}
	case err == pgx.ErrNoRows:
		// No linked account yet; proceed with the username from the interaction.
	default:
		slog.WarnContext(ctx, "discord_service: oauth identity lookup failed, proceeding without link",
			"discord_user_id", discordUserID,
			"error", err,
		)
	}

	id, err := uuid.NewV7()
	if err != nil {
		return db.DiscordRegistration{}, fmt.Errorf("generate registration id: %w", err)
	}

	reg, err := s.q.UpsertDiscordRegistration(ctx, db.UpsertDiscordRegistrationParams{
		ID:              id,
		DiscordUserID:   discordUserID,
		DiscordUsername: discordUsername,
		GuildID:         guildID,
		GuildName:       guildName,
	})
	if err != nil {
		return db.DiscordRegistration{}, fmt.Errorf("upsert discord registration: %w", err)
	}

	if linkedUserID.Valid {
		if err := s.q.LinkDiscordRegistrationToUser(ctx, db.LinkDiscordRegistrationToUserParams{
			UserID:        linkedUserID,
			DiscordUserID: discordUserID,
		}); err != nil {
			return db.DiscordRegistration{}, fmt.Errorf("link discord registration to user: %w", err)
		}
	}

	return reg, nil
}

// SetChannel stores (or updates) the designated Concept2 reporting channel for
// a guild, including its display name — shown on the guild's dashboard page
// so the channel is human-identifiable there without another Discord API
// call. The record is keyed on guild_id (unique); a repeated call updates the
// stored channel in place. guildID, channelID, and setByUserID must all be
// non-empty or ErrMissingGuildChannel is returned.
func (s *DiscordService) SetChannel(ctx context.Context, guildID, guildName, channelID, channelName, setByUserID string) (db.DiscordGuildSetting, error) {
	if guildID == "" || channelID == "" || setByUserID == "" {
		return db.DiscordGuildSetting{}, ErrMissingGuildChannel
	}

	if err := s.RecordGuildSeen(ctx, guildID, guildName); err != nil {
		slog.WarnContext(ctx, "discord_service: record guild seen failed, proceeding with set channel",
			"guild_id", guildID,
			"error", err,
		)
	}

	id, err := uuid.NewV7()
	if err != nil {
		return db.DiscordGuildSetting{}, fmt.Errorf("generate guild settings id: %w", err)
	}

	setting, err := s.q.UpsertGuildSettings(ctx, db.UpsertGuildSettingsParams{
		ID:              id,
		GuildID:         guildID,
		ReportChannelID: channelID,
		ChannelName:     channelName,
		SetByUserID:     setByUserID,
	})
	if err != nil {
		return db.DiscordGuildSetting{}, fmt.Errorf("upsert guild settings: %w", err)
	}

	return setting, nil
}

// ListRegisteredServers returns every Discord server the given site user has
// registered in via /register, ordered by when they registered.
func (s *DiscordService) ListRegisteredServers(ctx context.Context, userID uuid.UUID) ([]db.DiscordRegistration, error) {
	regs, err := s.q.ListDiscordRegistrationsByUser(ctx, pgtype.UUID{
		Bytes: userID,
		Valid: true,
	})
	if err != nil {
		return nil, fmt.Errorf("list discord registrations: %w", err)
	}
	return regs, nil
}

// CountRegisteredUsers returns how many users are currently registered in
// guildID — shown to server managers on the guild's dashboard page.
func (s *DiscordService) CountRegisteredUsers(ctx context.Context, guildID string) (int64, error) {
	count, err := s.q.CountDiscordRegistrationsByGuild(ctx, guildID)
	if err != nil {
		return 0, fmt.Errorf("count discord registrations by guild: %w", err)
	}
	return count, nil
}

// UnregisterFromGuild removes a user's registration (undoing
// RegisterFromInteraction / /register) for one specific guild, used by the
// guild-detail page's Unregister button. A no-op if no such registration
// exists — DELETE affecting zero rows is not an error.
func (s *DiscordService) UnregisterFromGuild(ctx context.Context, discordUserID, guildID string) error {
	if err := s.q.DeleteDiscordRegistration(ctx, db.DeleteDiscordRegistrationParams{
		DiscordUserID: discordUserID,
		GuildID:       guildID,
	}); err != nil {
		return fmt.Errorf("delete discord registration: %w", err)
	}
	return nil
}

// RemoveChannelSettings removes a guild's configured reporting channel. A no-op
// if no such settings row exists — DELETE affecting zero rows is not an error.
func (s *DiscordService) RemoveChannelSettings(ctx context.Context, guildID string) error {
	if err := s.q.DeleteGuildSettings(ctx, guildID); err != nil {
		return fmt.Errorf("delete guild settings: %w", err)
	}
	return nil
}

// ListConfiguredGuildIDs returns the set of guild IDs an admin has configured
// via /setchannel (i.e. that have a discord_guild_settings row). Used by the
// dashboard wizard's step-3 picker to filter down to servers that are
// actually ready to receive results, not just ones the bot happens to be
// installed in.
func (s *DiscordService) ListConfiguredGuildIDs(ctx context.Context) (map[string]struct{}, error) {
	ids, err := s.q.ListConfiguredGuildIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list configured guild ids: %w", err)
	}
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set, nil
}

// GetChannelSettings returns guildID's configured reporting channel, and
// false if no admin has run /setchannel there yet.
func (s *DiscordService) GetChannelSettings(ctx context.Context, guildID string) (db.DiscordGuildSetting, bool, error) {
	setting, err := s.q.GetGuildSettings(ctx, guildID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.DiscordGuildSetting{}, false, nil
		}
		return db.DiscordGuildSetting{}, false, fmt.Errorf("get guild settings: %w", err)
	}
	return setting, true, nil
}

// RecordGuildSeen upserts a guild's presence/name whenever we observe it —
// on bot install, or opportunistically from any slash-command interaction.
// There's no polling of Discord's guild list (this app scales to zero when
// idle), so this event-driven upsert is the only mechanism keeping
// discord_guilds current; call it liberally rather than trying to be clever
// about only calling it "when needed."
func (s *DiscordService) RecordGuildSeen(ctx context.Context, guildID, guildName string) error {
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate discord guild id: %w", err)
	}
	_, err = s.q.UpsertDiscordGuild(ctx, db.UpsertDiscordGuildParams{
		ID:        id,
		GuildID:   guildID,
		GuildName: guildName,
	})
	if err != nil {
		return fmt.Errorf("upsert discord guild: %w", err)
	}
	return nil
}
