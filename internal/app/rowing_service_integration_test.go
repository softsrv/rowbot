//go:build integration

package app_test

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/rowbot/internal/app"
	"github.com/softsrv/rowbot/internal/db"
	"github.com/softsrv/rowbot/internal/oauth"
	"github.com/softsrv/rowbot/internal/secrets"
)

// rowingFakes are the in-process stand-ins for Concept2 and Discord that a
// RowingService under test talks to, with the requests each one saw.
type rowingFakes struct {
	mu            sync.Mutex
	refreshCalls  int            // POST /oauth/access_token
	refreshTokens []string       // refresh_token form values received
	resultCalls   int            // GET /api/users/me/results/{id}
	resultAuth    []string       // Authorization headers on result fetches
	discordPosts  map[string]int // channel id -> POST /api/v10/channels/{id}/messages
	discordOther  int            // any other Discord request
	newAccess     string         // access token the fake refresh returns
	newRefresh    string         // refresh token the fake refresh returns
}

func (f *rowingFakes) concept2Handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/access_token":
			if err := r.ParseForm(); err != nil {
				t.Errorf("fake concept2: parse refresh form: %v", err)
			}
			f.refreshCalls++
			f.refreshTokens = append(f.refreshTokens, r.PostForm.Get("refresh_token"))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  f.newAccess,
				"refresh_token": f.newRefresh,
				"expires_in":    3600,
				"scope":         "user:read,results:read",
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/users/me/results/"):
			f.resultCalls++
			f.resultAuth = append(f.resultAuth, r.Header.Get("Authorization"))
			id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/users/me/results/"), 10, 64)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"id":           id,
				"date":         "2024-01-15 07:30:00",
				"distance":     2000,
				"type":         "rower",
				"time":         4200,
				"stroke_rate":  28,
				"workout_type": "FixedDistanceSplits",
				"workout": map[string]any{"splits": []map[string]any{
					{"time": 2100, "distance": 1000, "stroke_rate": 28},
					{"time": 2100, "distance": 1000, "stroke_rate": 28},
				}},
			}})
		default:
			t.Errorf("fake concept2: unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

func (f *rowingFakes) discordHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		rest, ok := strings.CutPrefix(r.URL.Path, "/api/v10/channels/")
		channelID, isMessages := strings.CutSuffix(rest, "/messages")
		if r.Method != http.MethodPost || !ok || !isMessages || channelID == "" || strings.Contains(channelID, "/") {
			f.discordOther++
			http.NotFound(w, r)
			return
		}
		f.discordPosts[channelID]++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1"}`))
	}
}

// snapshot returns copies of the counters, taken under the lock.
func (f *rowingFakes) snapshot() (refreshCalls, resultCalls, discordTotal int, posts map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	posts = make(map[string]int, len(f.discordPosts))
	for k, v := range f.discordPosts {
		posts[k] = v
		discordTotal += v
	}
	return f.refreshCalls, f.resultCalls, discordTotal + f.discordOther, posts
}

// newRowingHarness builds a RowingService on the real test database whose
// Concept2 apiBase is one httptest server and whose discord.com traffic is
// rewritten to another; no other host is reachable.
func newRowingHarness(t *testing.T) (*app.RowingService, *pgxpool.Pool, *secrets.Encrypter, *rowingFakes) {
	t.Helper()
	pool := testDB(t)
	enc := testEncrypter(t)
	fakes := &rowingFakes{
		discordPosts: map[string]int{},
		newAccess:    "c2-access-new-" + randSuffix(t),
		newRefresh:   "c2-refresh-new-" + randSuffix(t),
	}
	c2 := httptest.NewServer(fakes.concept2Handler(t))
	t.Cleanup(c2.Close)
	dc := httptest.NewServer(fakes.discordHandler())
	t.Cleanup(dc.Close)

	client := newRewriteClient(t, map[string]string{"discord.com": dc.URL, "log.concept2.com": c2.URL})
	c2Client := oauth.NewConcept2Client("client-id", "client-secret", "http://localhost/callback", c2.URL, client)
	svc := app.NewRowingService(db.New(pool), c2Client, enc, "bot-token", client)
	return svc, pool, enc, fakes
}

// randConcept2ID returns a random positive Concept2 user id, so concurrent
// and repeated runs never collide on oauth_identities (provider, provider_user_id).
func randConcept2ID(t *testing.T) int64 {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("randConcept2ID: %v", err)
	}
	return int64(binary.BigEndian.Uint64(b)>>2) + 1
}

// seedConcept2User seeds a user linked to a fresh Concept2 id and returns both.
func seedConcept2User(t *testing.T, pool *pgxpool.Pool) (db.User, db.OauthIdentity, int64) {
	t.Helper()
	user := seedUser(t, pool)
	c2ID := randConcept2ID(t)
	identity := seedOAuthIdentity(t, pool, user.ID, "concept2", strconv.FormatInt(c2ID, 10))
	return user, identity, c2ID
}

func TestIntegration_RowingService_ProcessResult_DiscardsWithoutConcept2Identity(t *testing.T) {
	svc, pool, _, fakes := newRowingHarness(t)
	// A user exists, but nobody is linked to this Concept2 id.
	seedUser(t, pool)

	if err := svc.ProcessResult(context.Background(), randConcept2ID(t), 101); err != nil {
		t.Fatalf("ProcessResult = %v; want nil (discard)", err)
	}
	refresh, results, discord, _ := fakes.snapshot()
	if refresh+results != 0 || discord != 0 {
		t.Fatalf("requests: concept2 refresh=%d result=%d, discord=%d; want none", refresh, results, discord)
	}
}

func TestIntegration_RowingService_ProcessResult_DiscardsWithoutToken(t *testing.T) {
	svc, pool, _, fakes := newRowingHarness(t)
	user, _, c2ID := seedConcept2User(t, pool)
	// Registered with a configured channel: only the missing token stops it.
	guild := seedDiscordGuild(t, pool)
	seedGuildSettings(t, pool, guild.GuildID)
	seedDiscordRegistration(t, pool, guild.GuildID, "", user.ID)

	if err := svc.ProcessResult(context.Background(), c2ID, 102); err != nil {
		t.Fatalf("ProcessResult = %v; want nil (discard)", err)
	}
	refresh, results, discord, _ := fakes.snapshot()
	if refresh+results != 0 || discord != 0 {
		t.Fatalf("requests: concept2 refresh=%d result=%d, discord=%d; want none", refresh, results, discord)
	}
}

// The result is fetched before registrations are consulted (rowing_service.go
// step 5 precedes step 6), so the discard is "nothing posted, no refresh".
func TestIntegration_RowingService_ProcessResult_DiscardsWithoutRegistrations(t *testing.T) {
	svc, pool, enc, fakes := newRowingHarness(t)
	_, identity, c2ID := seedConcept2User(t, pool)
	seedOAuthToken(t, pool, enc, identity.ID, "c2-access-"+randSuffix(t), "c2-refresh-"+randSuffix(t), time.Now().Add(time.Hour))
	// A registration in a configured guild that belongs to someone else.
	other := seedUser(t, pool)
	guild := seedDiscordGuild(t, pool)
	seedGuildSettings(t, pool, guild.GuildID)
	seedDiscordRegistration(t, pool, guild.GuildID, "", other.ID)

	if err := svc.ProcessResult(context.Background(), c2ID, 103); err != nil {
		t.Fatalf("ProcessResult = %v; want nil (discard)", err)
	}
	refresh, results, discord, _ := fakes.snapshot()
	if refresh != 0 || discord != 0 {
		t.Fatalf("requests: concept2 refresh=%d, discord=%d; want none", refresh, discord)
	}
	if results != 1 {
		t.Fatalf("concept2 result fetches = %d; want 1", results)
	}
}

func TestIntegration_RowingService_ProcessResult_DiscardsWithoutGuildSettings(t *testing.T) {
	svc, pool, enc, fakes := newRowingHarness(t)
	user, identity, c2ID := seedConcept2User(t, pool)
	seedOAuthToken(t, pool, enc, identity.ID, "c2-access-"+randSuffix(t), "c2-refresh-"+randSuffix(t), time.Now().Add(time.Hour))
	discordUser := "discord-user-" + randSuffix(t)
	for range 2 {
		guild := seedDiscordGuild(t, pool) // no settings for either guild
		seedDiscordRegistration(t, pool, guild.GuildID, discordUser, user.ID)
	}

	if err := svc.ProcessResult(context.Background(), c2ID, 104); err != nil {
		t.Fatalf("ProcessResult = %v; want nil (discard)", err)
	}
	refresh, results, discord, _ := fakes.snapshot()
	if refresh != 0 || discord != 0 {
		t.Fatalf("requests: concept2 refresh=%d, discord=%d; want none", refresh, discord)
	}
	if results != 1 {
		t.Fatalf("concept2 result fetches = %d; want 1", results)
	}
}

func TestIntegration_RowingService_ProcessResult_RefreshesExpiredToken(t *testing.T) {
	svc, pool, enc, fakes := newRowingHarness(t)
	_, identity, c2ID := seedConcept2User(t, pool)
	oldRefresh := "c2-refresh-old-" + randSuffix(t)
	oldExpiry := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	seedOAuthToken(t, pool, enc, identity.ID, "c2-access-old-"+randSuffix(t), oldRefresh, oldExpiry)

	before := time.Now()
	if err := svc.ProcessResult(context.Background(), c2ID, 105); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}

	fakes.mu.Lock()
	refreshCalls, sentRefresh, auth := fakes.refreshCalls, append([]string(nil), fakes.refreshTokens...), append([]string(nil), fakes.resultAuth...)
	fakes.mu.Unlock()
	if refreshCalls != 1 || len(sentRefresh) != 1 || sentRefresh[0] != oldRefresh {
		t.Fatalf("refresh calls = %d with tokens %q; want 1 with %q", refreshCalls, sentRefresh, oldRefresh)
	}
	if len(auth) != 1 || auth[0] != "Bearer "+fakes.newAccess {
		t.Fatalf("result fetch Authorization = %q; want the refreshed token", auth)
	}

	stored, err := db.New(pool).GetOAuthTokenByIdentity(context.Background(), identity.ID)
	if err != nil {
		t.Fatalf("re-read oauth_tokens: %v", err)
	}
	if !stored.ExpiresAt.Valid || stored.ExpiresAt.Time.Equal(oldExpiry) {
		t.Fatalf("expires_at = %v (valid=%v); want changed from %v", stored.ExpiresAt.Time, stored.ExpiresAt.Valid, oldExpiry)
	}
	if lo, hi := before.Add(3590*time.Second), time.Now().Add(3610*time.Second); stored.ExpiresAt.Time.Before(lo) || stored.ExpiresAt.Time.After(hi) {
		t.Fatalf("expires_at = %v; want about one hour from now (%v..%v)", stored.ExpiresAt.Time, lo, hi)
	}
	access, err := enc.Decrypt(stored.AccessTokenEnc)
	if err != nil || string(access) != fakes.newAccess {
		t.Fatalf("stored access token = %q, %v; want %q", access, err, fakes.newAccess)
	}
	refresh, err := enc.Decrypt(stored.RefreshTokenEnc)
	if err != nil || string(refresh) != fakes.newRefresh {
		t.Fatalf("stored refresh token = %q, %v; want %q", refresh, err, fakes.newRefresh)
	}
}

func TestIntegration_RowingService_ProcessResult_PostsOncePerChannel(t *testing.T) {
	svc, pool, enc, fakes := newRowingHarness(t)
	user, identity, c2ID := seedConcept2User(t, pool)
	seedOAuthToken(t, pool, enc, identity.ID, "c2-access-"+randSuffix(t), "c2-refresh-"+randSuffix(t), time.Now().Add(time.Hour))

	discordUser := "discord-user-" + randSuffix(t)
	want := map[string]int{}
	for range 2 {
		guild := seedDiscordGuild(t, pool)
		want[seedGuildSettings(t, pool, guild.GuildID).ReportChannelID] = 1
		seedDiscordRegistration(t, pool, guild.GuildID, discordUser, user.ID)
	}
	// A third guild the user is in, with no report channel: nothing goes there.
	unconfigured := seedDiscordGuild(t, pool)
	seedDiscordRegistration(t, pool, unconfigured.GuildID, discordUser, user.ID)
	// Another user's configured guild must not receive this user's result.
	otherGuild := seedDiscordGuild(t, pool)
	seedGuildSettings(t, pool, otherGuild.GuildID)
	seedDiscordRegistration(t, pool, otherGuild.GuildID, "", seedUser(t, pool).ID)

	if err := svc.ProcessResult(context.Background(), c2ID, 106); err != nil {
		t.Fatalf("ProcessResult: %v", err)
	}
	refresh, results, discord, posts := fakes.snapshot()
	if refresh != 0 || results != 1 {
		t.Fatalf("concept2 refresh=%d result=%d; want 0 and 1", refresh, results)
	}
	if discord != len(want) || len(posts) != len(want) {
		t.Fatalf("discord requests = %d, posts per channel = %v; want exactly %v", discord, posts, want)
	}
	for ch, n := range want {
		if posts[ch] != n {
			t.Fatalf("posts per channel = %v; want exactly %v", posts, want)
		}
	}
}
