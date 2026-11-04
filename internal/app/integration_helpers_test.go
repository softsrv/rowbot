//go:build integration

package app_test

// Shared helpers for the real-database (integration-tagged) tests in this
// package. Every seeder inserts rows with random-suffixed identifiers and
// registers its own t.Cleanup that deletes what it inserted, reporting a
// failed DELETE with t.Errorf, so tests can run repeatedly and side by side
// against the same *_test database without colliding or leaving rows behind.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/rowbot/internal/auth"
	"github.com/softsrv/rowbot/internal/db"
	"github.com/softsrv/rowbot/internal/secrets"
)

// testDB connects to the database pointed to by TEST_DATABASE_URL.
// Run integration tests through make, which guards, migrates, then tests:
//
//	TEST_DATABASE_URL=postgres://.../<name>_test make test-integration
//
// (`make test-db-up` starts a disposable one and prints its URL.)
// A missing URL, an unreachable database or an unmigrated schema fails the
// test with a hint: an integration test never skips.
func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL is not set: run `make test-integration` with it pointing at a disposable *_test database (`make test-db-up` starts one)")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("database unreachable: invalid TEST_DATABASE_URL (`make test-db-up` prints a valid one): %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("database unreachable at TEST_DATABASE_URL (is it running? `make test-db-up`): %v", err)
	}
	var missing bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('public.users') IS NULL").Scan(&missing); err != nil {
		t.Fatalf("schema check failed (start a test database with `make test-db-up`, then run `make test-integration`): %v", err)
	}
	if missing {
		t.Fatal("schema not migrated: table users is missing; run `make test-integration` (it applies migrations via test-db-migrate; `make test-db-up` starts a database)")
	}
	return pool
}

// mustCleanupDelete registers a t.Cleanup that runs one DELETE and reports its
// error: a failed cleanup would leave rows behind for later tests.
func mustCleanupDelete(t *testing.T, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), query, args...); err != nil {
			t.Errorf("cleanup %q: %v", query, err)
		}
	})
}

// randSuffix returns 16 random hex characters for building unique test
// identifiers (emails, provider user ids, guild and channel ids).
func randSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("randSuffix: %v", err)
	}
	return hex.EncodeToString(b)
}

// newID returns a fresh UUIDv7 or fails the test.
func newID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}
	return id
}

// seedUser is the package's only user seeder. It inserts a user with a fresh
// id and a random-suffixed email via db.Queries.CreateUser (the same query the
// Discord OAuth provisioning path uses) and registers a t.Cleanup that deletes
// the user — cascading its refresh tokens, verification codes, OAuth
// identities, OAuth tokens and linked Discord registrations.
func seedUser(t *testing.T, pool *pgxpool.Pool) db.User {
	t.Helper()
	id := newID(t)
	user, err := db.New(pool).CreateUser(context.Background(), db.CreateUserParams{
		ID:    id,
		Email: fmt.Sprintf("it-%s@example.com", randSuffix(t)),
	})
	if err != nil {
		t.Fatalf("seedUser CreateUser: %v", err)
	}
	mustCleanupDelete(t, pool, "DELETE FROM users WHERE id = $1", user.ID)
	return user
}

// mustIssueRefreshToken inserts a refresh token row (valid for 30 days) for
// userID directly and returns its raw (unhashed) value, letting tests exercise
// Refresh/Logout/sessions without a full OAuth login. The row is deleted by
// its own t.Cleanup (and by the user's cascade).
func mustIssueRefreshToken(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) string {
	t.Helper()
	id := newID(t)
	raw, hashed, err := auth.GenerateRefreshToken()
	if err != nil {
		t.Fatalf("generate refresh token: %v", err)
	}
	_, err = db.New(pool).InsertRefreshToken(context.Background(), db.InsertRefreshTokenParams{
		ID:        id,
		UserID:    userID,
		TokenHash: hashed,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(720 * time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertRefreshToken: %v", err)
	}
	mustCleanupDelete(t, pool, "DELETE FROM refresh_tokens WHERE id = $1", id)
	return raw
}

// seedOAuthIdentity links userID to provider ("discord" or "concept2") with a
// random-suffixed provider_user_id and provider_username, and registers a
// t.Cleanup deleting the identity by id (cascading its oauth_tokens row).
// Pass providerUserID == "" to get a random one; Concept2 ids are numeric in
// production, so pass a strconv.Itoa value when the code under test parses it.
func seedOAuthIdentity(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, provider, providerUserID string) db.OauthIdentity {
	t.Helper()
	suffix := randSuffix(t)
	if providerUserID == "" {
		providerUserID = provider + "-" + suffix
	}
	identity, err := db.New(pool).CreateOAuthIdentity(context.Background(), db.CreateOAuthIdentityParams{
		ID:               newID(t),
		UserID:           userID,
		Provider:         provider,
		ProviderUserID:   providerUserID,
		ProviderUsername: pgtype.Text{String: "user-" + suffix, Valid: true},
	})
	if err != nil {
		t.Fatalf("seedOAuthIdentity CreateOAuthIdentity: %v", err)
	}
	mustCleanupDelete(t, pool, "DELETE FROM oauth_identities WHERE id = $1", identity.ID)
	return identity
}

// seedOAuthToken stores accessToken and refreshToken (empty = NULL) for
// identityID, encrypted with enc exactly as OAuthService/RowingService store
// them, expiring at expiresAt (zero = NULL). A t.Cleanup deletes the row by
// its oauth_identity_id.
func seedOAuthToken(t *testing.T, pool *pgxpool.Pool, enc *secrets.Encrypter, identityID uuid.UUID, accessToken, refreshToken string, expiresAt time.Time) db.OauthToken {
	t.Helper()
	accessEnc, err := enc.Encrypt([]byte(accessToken))
	if err != nil {
		t.Fatalf("seedOAuthToken encrypt access: %v", err)
	}
	var refreshEnc []byte
	if refreshToken != "" {
		if refreshEnc, err = enc.Encrypt([]byte(refreshToken)); err != nil {
			t.Fatalf("seedOAuthToken encrypt refresh: %v", err)
		}
	}
	tok, err := db.New(pool).UpsertOAuthToken(context.Background(), db.UpsertOAuthTokenParams{
		ID:              newID(t),
		OauthIdentityID: identityID,
		AccessTokenEnc:  accessEnc,
		RefreshTokenEnc: refreshEnc,
		ExpiresAt:       pgtype.Timestamptz{Time: expiresAt, Valid: !expiresAt.IsZero()},
	})
	if err != nil {
		t.Fatalf("seedOAuthToken UpsertOAuthToken: %v", err)
	}
	mustCleanupDelete(t, pool, "DELETE FROM oauth_tokens WHERE oauth_identity_id = $1", identityID)
	return tok
}

// seedDiscordGuild records a guild the bot is installed in, with a random
// guild_id. discord_guilds has no cascade from users, so its t.Cleanup deletes
// the row by the seeded guild_id.
func seedDiscordGuild(t *testing.T, pool *pgxpool.Pool) db.DiscordGuild {
	t.Helper()
	suffix := randSuffix(t)
	guild, err := db.New(pool).UpsertDiscordGuild(context.Background(), db.UpsertDiscordGuildParams{
		ID:        newID(t),
		GuildID:   "guild-" + suffix,
		GuildName: "Guild " + suffix,
	})
	if err != nil {
		t.Fatalf("seedDiscordGuild UpsertDiscordGuild: %v", err)
	}
	mustCleanupDelete(t, pool, "DELETE FROM discord_guilds WHERE guild_id = $1", guild.GuildID)
	return guild
}

// seedGuildSettings sets guildID's report channel to a random channel id.
// discord_guild_settings has no cascade from users, so its t.Cleanup deletes
// the row by guildID.
func seedGuildSettings(t *testing.T, pool *pgxpool.Pool, guildID string) db.DiscordGuildSetting {
	t.Helper()
	suffix := randSuffix(t)
	settings, err := db.New(pool).UpsertGuildSettings(context.Background(), db.UpsertGuildSettingsParams{
		ID:              newID(t),
		GuildID:         guildID,
		ReportChannelID: "channel-" + suffix,
		ChannelName:     "training-" + suffix,
		SetByUserID:     "discord-admin-" + suffix,
	})
	if err != nil {
		t.Fatalf("seedGuildSettings UpsertGuildSettings: %v", err)
	}
	mustCleanupDelete(t, pool, "DELETE FROM discord_guild_settings WHERE guild_id = $1", guildID)
	return settings
}

// seedDiscordRegistration registers discordUserID (empty = random) in guildID
// and, when userID is not uuid.Nil, links the registration to that user. The
// registration is deleted by its own id in a t.Cleanup, since an unlinked
// registration has no cascade from users.
func seedDiscordRegistration(t *testing.T, pool *pgxpool.Pool, guildID, discordUserID string, userID uuid.UUID) db.DiscordRegistration {
	t.Helper()
	ctx := context.Background()
	q := db.New(pool)
	suffix := randSuffix(t)
	if discordUserID == "" {
		discordUserID = "discord-user-" + suffix
	}
	reg, err := q.UpsertDiscordRegistration(ctx, db.UpsertDiscordRegistrationParams{
		ID:              newID(t),
		DiscordUserID:   discordUserID,
		DiscordUsername: "discord-name-" + suffix,
		GuildID:         guildID,
		GuildName:       "Guild " + suffix,
	})
	if err != nil {
		t.Fatalf("seedDiscordRegistration UpsertDiscordRegistration: %v", err)
	}
	mustCleanupDelete(t, pool, "DELETE FROM discord_registrations WHERE id = $1", reg.ID)
	if userID != uuid.Nil {
		if err := q.LinkDiscordRegistrationToUser(ctx, db.LinkDiscordRegistrationToUserParams{
			UserID:        pgtype.UUID{Bytes: userID, Valid: true},
			DiscordUserID: discordUserID,
		}); err != nil {
			t.Fatalf("seedDiscordRegistration LinkDiscordRegistrationToUser: %v", err)
		}
		reg.UserID = pgtype.UUID{Bytes: userID, Valid: true}
	}
	return reg
}

// testEncrypter returns a secrets.Encrypter with a random 32-byte key, for
// constructing OAuthService/RowingService and seeding oauth_tokens.
func testEncrypter(t *testing.T) *secrets.Encrypter {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("testEncrypter key: %v", err)
	}
	enc, err := secrets.New(key)
	if err != nil {
		t.Fatalf("testEncrypter secrets.New: %v", err)
	}
	return enc
}

// rewriteTransport sends requests for each listed host (e.g. "discord.com") to
// an in-process httptest server, keeping the path and query, and lets requests
// addressed directly to one of those servers through. Any other host gets an
// error: it never falls through to http.DefaultTransport, so an integration
// test cannot reach the real Discord or Concept2.
type rewriteTransport struct {
	routes  map[string]*url.URL // listed host -> httptest server base URL
	allowed map[string]bool     // httptest server hosts (host:port)
	base    *http.Transport     // private transport used only for allowed targets
}

// RoundTrip implements http.RoundTripper.
func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target, routed := rt.routes[req.URL.Hostname()]
	if !routed && !rt.allowed[req.URL.Host] {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("rewriteTransport: request to unlisted host %q refused (integration tests must not reach external services)", req.URL.Host)
	}
	out := req.Clone(req.Context())
	if routed {
		out.URL.Scheme = target.Scheme
		out.URL.Host = target.Host
		out.Host = target.Host
	}
	return rt.base.RoundTrip(out)
}

// newRewriteClient returns an *http.Client whose rewriteTransport sends each
// routes key (a host such as "discord.com") to its value (an httptest server
// URL, e.g. srv.URL). Requests straight to a routes value are allowed (so a
// Concept2 apiBase of srv.URL works with the same client); every other host
// errors. Idle connections are closed on test cleanup.
func newRewriteClient(t *testing.T, routes map[string]string) *http.Client {
	t.Helper()
	rt := &rewriteTransport{
		routes:  make(map[string]*url.URL, len(routes)),
		allowed: make(map[string]bool, len(routes)),
		base:    &http.Transport{},
	}
	for host, raw := range routes {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			t.Fatalf("newRewriteClient: bad target URL %q for host %q: %v", raw, host, err)
		}
		rt.routes[host] = u
		rt.allowed[u.Host] = true
	}
	t.Cleanup(rt.base.CloseIdleConnections)
	return &http.Client{Transport: rt, Timeout: 10 * time.Second}
}

// TestIntegration_RewriteTransport pins the guarantee every new test relies
// on: a listed host is served by the httptest server, and any other host is
// refused instead of reaching the network.
func TestIntegration_RewriteTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fake:" + r.URL.Path))
	}))
	t.Cleanup(srv.Close)
	client := newRewriteClient(t, map[string]string{"discord.com": srv.URL})

	resp, err := client.Get("https://discord.com/api/users/@me")
	if err != nil {
		t.Fatalf("listed host: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(body) != "fake:/api/users/@me" {
		t.Fatalf("listed host body = %q, %v; want fake:/api/users/@me", body, err)
	}

	for _, u := range []string{"https://log.concept2.com/api/users/me", "https://example.com/", "https://cdn.discord.com/x"} {
		resp, err := client.Get(u)
		if err == nil {
			_ = resp.Body.Close()
			t.Errorf("GET %s succeeded; want refused", u)
			continue
		}
		if !strings.Contains(err.Error(), "rewriteTransport: request to unlisted host") {
			t.Errorf("GET %s error = %v; want the transport's refusal, not a network error", u, err)
		}
	}
}
