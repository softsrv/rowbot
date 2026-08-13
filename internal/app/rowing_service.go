package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/softsrv/rowbot/internal/concept2"
	"github.com/softsrv/rowbot/internal/db"
	"github.com/softsrv/rowbot/internal/discord"
	"github.com/softsrv/rowbot/internal/oauth"
	"github.com/softsrv/rowbot/internal/render"
	"github.com/softsrv/rowbot/internal/secrets"
)

// RowingService processes incoming Concept2 workout results and posts them to
// the appropriate Discord reporting channels.
type RowingService struct {
	q          *db.Queries
	concept2   *oauth.Concept2Client
	encrypter  *secrets.Encrypter
	botToken   string
	httpClient *http.Client
}

// NewRowingService constructs a RowingService. concept2Client and encrypter may
// be nil (ProcessResult will fail fast with a clear error if they are needed but
// absent). Pass nil for httpClient to use http.DefaultClient.
func NewRowingService(q *db.Queries, concept2Client *oauth.Concept2Client, encrypter *secrets.Encrypter, botToken string, httpClient *http.Client) *RowingService {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &RowingService{
		q:          q,
		concept2:   concept2Client,
		encrypter:  encrypter,
		botToken:   botToken,
		httpClient: httpClient,
	}
}

// ProcessResult resolves a Concept2 webhook result through the full pipeline:
//  1. Look up the local user linked to the Concept2 account.
//  2. Retrieve and decrypt the stored OAuth token (refreshing if expired).
//  3. Fetch the result from the Concept2 API.
//  4. Find all Discord guilds the user is registered in with a configured
//     reporting channel.
//  5. Render the result image once.
//  6. Post it to every guild's reporting channel concurrently — the sends
//     are independent Discord API calls with no ordering dependency.
//
// Business-logic discards (no linked user, no token, no Discord registrations,
// no guild settings) return nil — only unexpected failures return errors.
func (s *RowingService) ProcessResult(ctx context.Context, concept2UserID int64, resultID int64) error {
	// 1. Look up oauth_identity for this Concept2 user.
	identity, err := s.q.GetOAuthIdentityByProviderUserID(ctx, db.GetOAuthIdentityByProviderUserIDParams{
		Provider:       "concept2",
		ProviderUserID: strconv.FormatInt(concept2UserID, 10),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.Info("concept2 result: no linked user, discarding",
				"concept2_user_id", concept2UserID,
			)
			return nil
		}
		return fmt.Errorf("concept2 result: lookup identity: %w", err)
	}

	// 2. Get the stored encrypted OAuth token for this identity.
	token, err := s.q.GetOAuthTokenByIdentity(ctx, identity.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.Info("concept2 result: no stored token for user, discarding",
				"concept2_user_id", concept2UserID,
				"identity_id", identity.ID,
			)
			return nil
		}
		return fmt.Errorf("concept2 result: get token: %w", err)
	}

	// 3. Decrypt the access token.
	accessPlain, err := s.encrypter.Decrypt(token.AccessTokenEnc)
	if err != nil {
		return fmt.Errorf("concept2 result: decrypt access token: %w", err)
	}
	accessToken := string(accessPlain)

	// 4. Refresh token if expired or expiring within 60 seconds.
	if token.ExpiresAt.Valid && time.Until(token.ExpiresAt.Time) <= 60*time.Second {
		slog.Debug("concept2 result: token expired or expiring soon, refreshing",
			"identity_id", identity.ID,
			"expires_at", token.ExpiresAt.Time,
		)

		if len(token.RefreshTokenEnc) == 0 {
			return fmt.Errorf("concept2 result: token is expired and no refresh token stored")
		}

		refreshPlain, decErr := s.encrypter.Decrypt(token.RefreshTokenEnc)
		if decErr != nil {
			return fmt.Errorf("concept2 result: decrypt refresh token: %w", decErr)
		}

		newTok, refreshErr := s.concept2.RefreshToken(ctx, string(refreshPlain))
		if refreshErr != nil {
			slog.Error("concept2 result: token refresh failed",
				"identity_id", identity.ID,
				"error", refreshErr,
			)
			return fmt.Errorf("concept2 result: refresh token: %w", refreshErr)
		}

		// Re-encrypt and persist the refreshed token pair.
		if storeErr := s.storeRefreshedToken(ctx, identity.ID, newTok.AccessToken, newTok.RefreshToken, newTok.Scope, newTok.ExpiresIn); storeErr != nil {
			return fmt.Errorf("concept2 result: persist refreshed token: %w", storeErr)
		}
		accessToken = newTok.AccessToken
	}

	// 5. Fetch the result from the Concept2 API.
	result, err := concept2.GetResult(ctx, s.httpClient, s.concept2.APIBase(), accessToken, resultID)
	if err != nil {
		return fmt.Errorf("concept2 result: fetch result: %w", err)
	}

	// 6. Find Discord registrations for this user.
	registrations, err := s.q.ListDiscordRegistrationsByUser(ctx, pgtype.UUID{
		Bytes: identity.UserID,
		Valid: true,
	})
	if err != nil {
		return fmt.Errorf("concept2 result: list discord registrations: %w", err)
	}
	if len(registrations) == 0 {
		slog.Info("concept2 result: user has no discord registrations, discarding",
			"user_id", identity.UserID,
			"result_id", resultID,
		)
		return nil
	}

	// 7. Resolve which channels actually need the result — guilds without a
	// configured report channel are skipped — before rendering anything, so a
	// user with no valid targets doesn't pay for a render that's never used.
	// Deduplicated by (guild, channel) rather than trusting that registrations
	// can never resolve to the same channel twice — discord_guild_settings.
	// guild_id is UNIQUE today, so that can't currently happen, but this way
	// the send loop doesn't depend on that constraint holding forever to avoid
	// posting the same result twice to one channel.
	type sendTarget struct {
		guildID       string
		discordUserID string
		channelID     string
	}
	seen := make(map[string]bool) // keyed by guildID+"|"+channelID
	var targets []sendTarget
	for _, reg := range registrations {
		settings, settingsErr := s.q.GetGuildSettings(ctx, reg.GuildID)
		if settingsErr != nil {
			if errors.Is(settingsErr, pgx.ErrNoRows) {
				slog.Debug("concept2 result: no guild settings configured, skipping guild",
					"guild_id", reg.GuildID,
				)
				continue
			}
			slog.Error("concept2 result: get guild settings failed",
				"guild_id", reg.GuildID,
				"error", settingsErr,
			)
			continue
		}

		key := reg.GuildID + "|" + settings.ReportChannelID
		if seen[key] {
			slog.Debug("concept2 result: duplicate guild+channel target, skipping",
				"guild_id", reg.GuildID,
				"channel_id", settings.ReportChannelID,
			)
			continue
		}
		seen[key] = true

		targets = append(targets, sendTarget{
			guildID:       reg.GuildID,
			discordUserID: reg.DiscordUserID,
			channelID:     settings.ReportChannelID,
		})
	}
	if len(targets) == 0 {
		slog.Info("concept2 result: no guilds with a configured report channel, discarding",
			"user_id", identity.UserID,
			"result_id", resultID,
		)
		return nil
	}

	// 8. Render once — every target guild posts the same image regardless of
	// which channel it ends up in.
	pngBytes, err := render.RenderResultPNG(result)
	if err != nil {
		return fmt.Errorf("render result image: %w", err)
	}

	// 9. Send to every target channel concurrently.
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t sendTarget) {
			defer wg.Done()
			if sendErr := s.sendResultImage(ctx, pngBytes, t.discordUserID, t.channelID); sendErr != nil {
				slog.Error("concept2 result: send discord image failed",
					"guild_id", t.guildID,
					"channel_id", t.channelID,
					"result_id", resultID,
					"error", sendErr,
				)
				return
			}
			slog.Info("concept2 result: discord image sent",
				"guild_id", t.guildID,
				"channel_id", t.channelID,
				"result_id", resultID,
			)
		}(t)
	}
	wg.Wait()

	return nil
}

// activityMessageFormat is the Discord message content format posted
// alongside every rendered result image. It @-mentions the Discord user who
// completed the activity using Discord's <@snowflake> mention syntax, which
// the client renders as a highlighted, pinging mention (plain "@username"
// text does not ping or render specially). The image itself (and its header
// band) carries the sport-specific details, so the message text stays
// generic and doesn't need to guess at the sport type.
const activityMessageFormat = "<@%s> has completed an activity!"

// sendResultImage posts a pre-rendered result image as a Discord message
// attachment, with message content that @-mentions the Discord user
// identified by discordUserID. pngBytes is shared read-only across
// concurrent calls (one per target guild) — safe, since none of them mutate it.
func (s *RowingService) sendResultImage(ctx context.Context, pngBytes []byte, discordUserID, channelID string) error {
	content := fmt.Sprintf(activityMessageFormat, discordUserID)

	return discord.SendChannelMessageWithAttachment(ctx, s.httpClient, s.botToken, channelID, content, discord.Attachment{
		Filename: "result.png",
		Data:     pngBytes,
	})
}

// storeRefreshedToken encrypts and upserts a refreshed token pair for the given identity.
func (s *RowingService) storeRefreshedToken(ctx context.Context, identityID uuid.UUID, accessToken, refreshToken, scope string, expiresIn int) error {
	accessEnc, err := s.encrypter.Encrypt([]byte(accessToken))
	if err != nil {
		return fmt.Errorf("encrypt access token: %w", err)
	}

	var refreshEnc []byte
	if refreshToken != "" {
		refreshEnc, err = s.encrypter.Encrypt([]byte(refreshToken))
		if err != nil {
			return fmt.Errorf("encrypt refresh token: %w", err)
		}
	}

	tokenID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate token id: %w", err)
	}

	var expiresAt pgtype.Timestamptz
	if expiresIn > 0 {
		expiresAt = pgtype.Timestamptz{
			Time:  time.Now().Add(time.Duration(expiresIn) * time.Second),
			Valid: true,
		}
	}

	_, err = s.q.UpsertOAuthToken(ctx, db.UpsertOAuthTokenParams{
		ID:              tokenID,
		OauthIdentityID: identityID,
		AccessTokenEnc:  accessEnc,
		RefreshTokenEnc: refreshEnc,
		Scope:           pgtype.Text{String: scope, Valid: scope != ""},
		ExpiresAt:       expiresAt,
	})
	return err
}
