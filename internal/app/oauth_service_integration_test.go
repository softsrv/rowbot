//go:build integration

package app_test

// Real-database tests for OAuthService. Postgres is real (TEST_DATABASE_URL);
// Discord and Concept2 are faked by in-process httptest servers reached
// through newRewriteClient, so no test ever touches a real provider host.

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/rowbot/internal/app"
	"github.com/softsrv/rowbot/internal/db"
	"github.com/softsrv/rowbot/internal/oauth"
	"github.com/softsrv/rowbot/internal/secrets"
)

// fakeProviders is one test's fake Discord and Concept2. Each issues random
// access/refresh tokens and answers the profile (and Discord guild list)
// endpoints only for its own access token.
type fakeProviders struct {
	discordUser    oauth.DiscordUser
	discordGuilds  []oauth.DiscordGuild
	discordAccess  string
	discordRefresh string

	c2User    oauth.Concept2User
	c2Access  string
	c2Refresh string
}

// oauthFixture is an OAuthService wired to a real pool and the fake providers.
type oauthFixture struct {
	pool  *pgxpool.Pool
	enc   *secrets.Encrypter
	svc   *app.OAuthService
	fakes *fakeProviders
}

func oauthWriteJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("fake provider encode: %v", err)
	}
}

func oauthBearerIs(r *http.Request, token string) bool {
	return r.Header.Get("Authorization") == "Bearer "+token
}

// newOAuthFixture builds OAuthService against the real test database, with
// Discord routed (by host) to one httptest server and Concept2's apiBase set
// to another httptest server's URL.
func newOAuthFixture(t *testing.T) *oauthFixture {
	t.Helper()
	pool := testDB(t)
	enc := testEncrypter(t)
	f := &fakeProviders{
		discordUser: oauth.DiscordUser{
			ID:       "discord-" + randSuffix(t),
			Username: "dname-" + randSuffix(t),
			Email:    "it-discord-" + randSuffix(t) + "@example.com",
			Verified: true,
		},
		discordAccess:  "d-access-" + randSuffix(t),
		discordRefresh: "d-refresh-" + randSuffix(t),
		c2User: oauth.Concept2User{
			ID:       rand.IntN(1<<30) + 1,
			Username: "c2-" + randSuffix(t),
		},
		c2Access:  "c2-access-" + randSuffix(t),
		c2Refresh: "c2-refresh-" + randSuffix(t),
	}

	discordSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/oauth2/token":
			oauthWriteJSON(t, w, oauth.DiscordTokenResponse{
				AccessToken: f.discordAccess, RefreshToken: f.discordRefresh,
				TokenType: "Bearer", ExpiresIn: 3600, Scope: "identify email guilds",
			})
		case r.URL.Path == "/api/users/@me" && oauthBearerIs(r, f.discordAccess):
			oauthWriteJSON(t, w, f.discordUser)
		case r.URL.Path == "/api/users/@me/guilds" && oauthBearerIs(r, f.discordAccess):
			oauthWriteJSON(t, w, f.discordGuilds)
		default:
			http.Error(w, "fake discord: unexpected request", http.StatusUnauthorized)
		}
	}))
	t.Cleanup(discordSrv.Close)

	c2Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/access_token":
			oauthWriteJSON(t, w, oauth.Concept2TokenResponse{
				AccessToken: f.c2Access, RefreshToken: f.c2Refresh,
				TokenType: "Bearer", ExpiresIn: 3600, Scope: "user:read,results:read",
			})
		case r.URL.Path == "/api/users/me" && oauthBearerIs(r, f.c2Access):
			oauthWriteJSON(t, w, map[string]any{"data": f.c2User})
		default:
			http.Error(w, "fake concept2: unexpected request", http.StatusUnauthorized)
		}
	}))
	t.Cleanup(c2Srv.Close)

	// Only discord.com is rewritten; the Concept2 server is listed so that
	// requests straight to its URL (the apiBase below) are allowed.
	client := newRewriteClient(t, map[string]string{
		"discord.com":      discordSrv.URL,
		"log.concept2.com": c2Srv.URL,
	})
	discord := oauth.NewDiscordClient("client-id", "client-secret", "http://localhost/cb", client)
	concept2 := oauth.NewConcept2Client("client-id", "client-secret", "http://localhost/cb", c2Srv.URL, client)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // stops the guild-membership cache sweeper
	svc := app.NewOAuthService(ctx, db.New(pool), pool, discord, concept2, enc, testAuthService(t, pool))
	return &oauthFixture{pool: pool, enc: enc, svc: svc, fakes: f}
}

func oauthCountRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// identityOwner returns the user_id linked to (provider, providerUserID), or
// uuid.Nil when no identity exists.
func oauthIdentityOwner(t *testing.T, pool *pgxpool.Pool, provider, providerUserID string) uuid.UUID {
	t.Helper()
	identity, err := db.New(pool).GetOAuthIdentityByProviderUserID(context.Background(), db.GetOAuthIdentityByProviderUserIDParams{
		Provider: provider, ProviderUserID: providerUserID,
	})
	if err != nil {
		return uuid.Nil
	}
	return identity.UserID
}

// assertStoredTokens checks the oauth_tokens row for (provider,
// providerUserID) holds ciphertext, not plaintext, that decrypts to the fake
// provider's access and refresh tokens.
func assertStoredTokens(t *testing.T, fx *oauthFixture, provider, providerUserID, access, refresh string) {
	t.Helper()
	ctx := context.Background()
	q := db.New(fx.pool)
	identity, err := q.GetOAuthIdentityByProviderUserID(ctx, db.GetOAuthIdentityByProviderUserIDParams{
		Provider: provider, ProviderUserID: providerUserID,
	})
	if err != nil {
		t.Fatalf("identity %s/%s: %v", provider, providerUserID, err)
	}
	tok, err := q.GetOAuthTokenByIdentity(ctx, identity.ID)
	if err != nil {
		t.Fatalf("oauth_tokens for %s/%s: %v", provider, providerUserID, err)
	}
	for _, c := range []struct {
		name, plain string
		enc         []byte
	}{{"access", access, tok.AccessTokenEnc}, {"refresh", refresh, tok.RefreshTokenEnc}} {
		if len(c.enc) == 0 || string(c.enc) == c.plain {
			t.Errorf("%s token stored as %q; want ciphertext of %q", c.name, c.enc, c.plain)
			continue
		}
		got, err := fx.enc.Decrypt(c.enc)
		if err != nil {
			t.Errorf("decrypt stored %s token: %v", c.name, err)
			continue
		}
		if string(got) != c.plain {
			t.Errorf("stored %s token decrypts to %q; want %q", c.name, got, c.plain)
		}
	}
}

func TestIntegration_OAuthService_DiscordCallback_ExistingLink(t *testing.T) {
	fx := newOAuthFixture(t)
	user := seedUser(t, fx.pool)
	seedOAuthIdentity(t, fx.pool, user.ID, "discord", fx.fakes.discordUser.ID)

	result, err := fx.svc.HandleDiscordCallback(context.Background(), "code", app.DeviceMeta{})
	if err != nil {
		t.Fatalf("HandleDiscordCallback: %v", err)
	}
	if result.AccessToken == "" || result.RefreshToken == "" {
		t.Error("expected a token pair for the linked user")
	}
	if got := oauthIdentityOwner(t, fx.pool, "discord", fx.fakes.discordUser.ID); got != user.ID {
		t.Errorf("identity user_id = %v; want %v", got, user.ID)
	}
	if n := oauthCountRows(t, fx.pool, "SELECT count(*) FROM refresh_tokens WHERE user_id = $1", user.ID); n != 1 {
		t.Errorf("refresh tokens for linked user = %d; want 1 (tokens issued to the existing user)", n)
	}
	if n := oauthCountRows(t, fx.pool, "SELECT count(*) FROM users WHERE email = $1", fx.fakes.discordUser.Email); n != 0 {
		t.Errorf("users with the Discord email = %d; want 0 (no new user provisioned)", n)
	}
	assertStoredTokens(t, fx, "discord", fx.fakes.discordUser.ID, fx.fakes.discordAccess, fx.fakes.discordRefresh)
}

func TestIntegration_OAuthService_DiscordCallback_VerifiedEmailAutoLinks(t *testing.T) {
	fx := newOAuthFixture(t)
	user := seedUser(t, fx.pool)
	fx.fakes.discordUser.Email = user.Email
	fx.fakes.discordUser.Verified = true

	if _, err := fx.svc.HandleDiscordCallback(context.Background(), "code", app.DeviceMeta{}); err != nil {
		t.Fatalf("HandleDiscordCallback: %v", err)
	}
	if got := oauthIdentityOwner(t, fx.pool, "discord", fx.fakes.discordUser.ID); got != user.ID {
		t.Errorf("identity user_id = %v; want existing user %v", got, user.ID)
	}
	if n := oauthCountRows(t, fx.pool, "SELECT count(*) FROM users WHERE email = $1", user.Email); n != 1 {
		t.Errorf("users with email = %d; want 1", n)
	}
	assertStoredTokens(t, fx, "discord", fx.fakes.discordUser.ID, fx.fakes.discordAccess, fx.fakes.discordRefresh)
}

func TestIntegration_OAuthService_DiscordCallback_ProvisionsNewUser(t *testing.T) {
	fx := newOAuthFixture(t)
	email := fx.fakes.discordUser.Email
	// The service creates this user; delete it (cascading identity, tokens and
	// refresh tokens) by its random email.
	mustCleanupDelete(t, fx.pool, "DELETE FROM users WHERE email = $1", email)

	result, err := fx.svc.HandleDiscordCallback(context.Background(), "code", app.DeviceMeta{})
	if err != nil {
		t.Fatalf("HandleDiscordCallback: %v", err)
	}
	newUser, err := db.New(fx.pool).GetUserByEmail(context.Background(), email)
	if err != nil {
		t.Fatalf("provisioned user not found by email %q: %v", email, err)
	}
	if got := oauthIdentityOwner(t, fx.pool, "discord", fx.fakes.discordUser.ID); got != newUser.ID {
		t.Errorf("identity user_id = %v; want provisioned user %v", got, newUser.ID)
	}
	if n := oauthCountRows(t, fx.pool, "SELECT count(*) FROM refresh_tokens WHERE user_id = $1", newUser.ID); n != 1 {
		t.Errorf("refresh tokens for provisioned user = %d; want 1", n)
	}
	if result.RefreshToken == "" {
		t.Error("expected a refresh token for the provisioned user")
	}
	assertStoredTokens(t, fx, "discord", fx.fakes.discordUser.ID, fx.fakes.discordAccess, fx.fakes.discordRefresh)
}

func TestIntegration_OAuthService_DiscordCallback_UnverifiedEmailRefused(t *testing.T) {
	fx := newOAuthFixture(t)
	user := seedUser(t, fx.pool)
	fx.fakes.discordUser.Email = user.Email
	fx.fakes.discordUser.Verified = false

	_, err := fx.svc.HandleDiscordCallback(context.Background(), "code", app.DeviceMeta{})
	if !errors.Is(err, app.ErrDiscordUnverifiedEmail) {
		t.Fatalf("HandleDiscordCallback err = %v; want ErrDiscordUnverifiedEmail", err)
	}
	if n := oauthCountRows(t, fx.pool, "SELECT count(*) FROM users WHERE email = $1", user.Email); n != 1 {
		t.Errorf("users with email = %d; want 1 (no user created)", n)
	}
	if n := oauthCountRows(t, fx.pool, "SELECT count(*) FROM oauth_identities WHERE provider = 'discord' AND provider_user_id = $1", fx.fakes.discordUser.ID); n != 0 {
		t.Errorf("discord identities = %d; want 0 (no link created)", n)
	}
	if n := oauthCountRows(t, fx.pool, "SELECT count(*) FROM refresh_tokens WHERE user_id = $1", user.ID); n != 0 {
		t.Errorf("refresh tokens = %d; want 0 (no session issued)", n)
	}
}

func TestIntegration_OAuthService_LinkDiscord(t *testing.T) {
	fx := newOAuthFixture(t)
	user := seedUser(t, fx.pool)

	if err := fx.svc.LinkDiscord(context.Background(), user.ID, "code"); err != nil {
		t.Fatalf("LinkDiscord: %v", err)
	}
	if got := oauthIdentityOwner(t, fx.pool, "discord", fx.fakes.discordUser.ID); got != user.ID {
		t.Errorf("identity user_id = %v; want caller %v", got, user.ID)
	}
	assertStoredTokens(t, fx, "discord", fx.fakes.discordUser.ID, fx.fakes.discordAccess, fx.fakes.discordRefresh)
}

func TestIntegration_OAuthService_LinkDiscord_AlreadyLinkedToOtherUser(t *testing.T) {
	fx := newOAuthFixture(t)
	owner := seedUser(t, fx.pool)
	caller := seedUser(t, fx.pool)
	seedOAuthIdentity(t, fx.pool, owner.ID, "discord", fx.fakes.discordUser.ID)

	err := fx.svc.LinkDiscord(context.Background(), caller.ID, "code")
	if !errors.Is(err, app.ErrDiscordAlreadyLinkedToOtherUser) {
		t.Fatalf("LinkDiscord err = %v; want ErrDiscordAlreadyLinkedToOtherUser", err)
	}
	if got := oauthIdentityOwner(t, fx.pool, "discord", fx.fakes.discordUser.ID); got != owner.ID {
		t.Errorf("identity user_id = %v; want unchanged owner %v", got, owner.ID)
	}
	if n := oauthCountRows(t, fx.pool, "SELECT count(*) FROM oauth_identities WHERE user_id = $1", caller.ID); n != 0 {
		t.Errorf("caller identities = %d; want 0", n)
	}
}

func TestIntegration_OAuthService_LinkConcept2(t *testing.T) {
	fx := newOAuthFixture(t)
	user := seedUser(t, fx.pool)
	c2ID := strconv.Itoa(fx.fakes.c2User.ID)

	if err := fx.svc.LinkConcept2(context.Background(), user.ID, "code"); err != nil {
		t.Fatalf("LinkConcept2: %v", err)
	}
	if got := oauthIdentityOwner(t, fx.pool, "concept2", c2ID); got != user.ID {
		t.Errorf("identity user_id = %v; want caller %v", got, user.ID)
	}
	assertStoredTokens(t, fx, "concept2", c2ID, fx.fakes.c2Access, fx.fakes.c2Refresh)
}

func TestIntegration_OAuthService_LinkConcept2_AlreadyLinkedToOtherUser(t *testing.T) {
	fx := newOAuthFixture(t)
	owner := seedUser(t, fx.pool)
	caller := seedUser(t, fx.pool)
	c2ID := strconv.Itoa(fx.fakes.c2User.ID)
	seedOAuthIdentity(t, fx.pool, owner.ID, "concept2", c2ID)

	err := fx.svc.LinkConcept2(context.Background(), caller.ID, "code")
	if !errors.Is(err, app.ErrConcept2AlreadyLinkedToOtherUser) {
		t.Fatalf("LinkConcept2 err = %v; want ErrConcept2AlreadyLinkedToOtherUser", err)
	}
	if got := oauthIdentityOwner(t, fx.pool, "concept2", c2ID); got != owner.ID {
		t.Errorf("identity user_id = %v; want unchanged owner %v", got, owner.ID)
	}
	if n := oauthCountRows(t, fx.pool, "SELECT count(*) FROM oauth_identities WHERE user_id = $1", caller.ID); n != 0 {
		t.Errorf("caller identities = %d; want 0", n)
	}
}

func TestIntegration_OAuthService_UnlinkConcept2(t *testing.T) {
	fx := newOAuthFixture(t)
	user := seedUser(t, fx.pool)
	identity := seedOAuthIdentity(t, fx.pool, user.ID, "concept2", strconv.Itoa(fx.fakes.c2User.ID))
	seedOAuthToken(t, fx.pool, fx.enc, identity.ID, fx.fakes.c2Access, fx.fakes.c2Refresh, time.Time{})
	discord := seedOAuthIdentity(t, fx.pool, user.ID, "discord", "")

	if err := fx.svc.UnlinkConcept2(context.Background(), user.ID); err != nil {
		t.Fatalf("UnlinkConcept2: %v", err)
	}
	if n := oauthCountRows(t, fx.pool, "SELECT count(*) FROM oauth_identities WHERE id = $1", identity.ID); n != 0 {
		t.Errorf("concept2 identity rows = %d; want 0", n)
	}
	if n := oauthCountRows(t, fx.pool, "SELECT count(*) FROM oauth_tokens WHERE oauth_identity_id = $1", identity.ID); n != 0 {
		t.Errorf("concept2 token rows = %d; want 0", n)
	}
	if n := oauthCountRows(t, fx.pool, "SELECT count(*) FROM oauth_identities WHERE id = $1", discord.ID); n != 1 {
		t.Errorf("discord identity rows = %d; want 1 (only Concept2 is unlinked)", n)
	}
}

func TestIntegration_OAuthService_GetDiscordGuildMemberships(t *testing.T) {
	fx := newOAuthFixture(t)
	user := seedUser(t, fx.pool)
	identity := seedOAuthIdentity(t, fx.pool, user.ID, "discord", fx.fakes.discordUser.ID)
	seedOAuthToken(t, fx.pool, fx.enc, identity.ID, fx.fakes.discordAccess, fx.fakes.discordRefresh, time.Now().Add(time.Hour))

	installedAdmin := seedDiscordGuild(t, fx.pool)
	installedMember := seedDiscordGuild(t, fx.pool)
	seedDiscordGuild(t, fx.pool) // installed, but the user is not a member
	fx.fakes.discordGuilds = []oauth.DiscordGuild{
		{ID: installedAdmin.GuildID, Name: "Admin Guild", Permissions: 0x20},
		{ID: "not-installed-" + randSuffix(t), Name: "Other Guild", Owner: true},
		{ID: installedMember.GuildID, Name: "Member Guild"},
	}

	got, err := fx.svc.GetDiscordGuildMemberships(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("GetDiscordGuildMemberships: %v", err)
	}
	want := []app.GuildMembership{
		{GuildID: installedAdmin.GuildID, GuildName: "Admin Guild", IsAdmin: true},
		{GuildID: installedMember.GuildID, GuildName: "Member Guild", IsAdmin: false},
	}
	if !slices.Equal(got, want) {
		t.Errorf("memberships = %+v; want %+v (user's guilds intersected with discord_guilds)", got, want)
	}
}
