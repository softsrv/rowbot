package handlers_test

import (
	"context"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/softsrv/rowbot/internal/app"
	"github.com/softsrv/rowbot/internal/auth"
	"github.com/softsrv/rowbot/internal/db"
	"github.com/softsrv/rowbot/internal/discord"
	"github.com/softsrv/rowbot/internal/http/handlers"
	"github.com/softsrv/rowbot/internal/http/middleware"
	"github.com/softsrv/rowbot/web"
)

type fakeProfileUsers struct {
	user db.User
}

func (f *fakeProfileUsers) GetUserByID(context.Context, uuid.UUID) (db.User, error) {
	return f.user, nil
}

func (f *fakeProfileUsers) DeleteAccount(context.Context, uuid.UUID) error {
	return nil
}

func (f *fakeProfileUsers) SetSetupProgress(context.Context, uuid.UUID, int32) error {
	return nil
}

type fakeProfileOAuth struct {
	memberships []app.GuildMembership
	identity    db.OauthIdentity
}

func (f *fakeProfileOAuth) IsDiscordEnabled() bool  { return true }
func (f *fakeProfileOAuth) IsConcept2Enabled() bool { return false }
func (f *fakeProfileOAuth) GetDiscordIdentity(context.Context, uuid.UUID) (db.OauthIdentity, error) {
	return f.identity, nil
}
func (f *fakeProfileOAuth) GetConcept2Identity(context.Context, uuid.UUID) (db.OauthIdentity, error) {
	return db.OauthIdentity{}, nil
}
func (f *fakeProfileOAuth) GetDiscordGuildMemberships(context.Context, uuid.UUID) ([]app.GuildMembership, error) {
	return f.memberships, nil
}

type setChannelCall struct {
	guildID     string
	guildName   string
	channelID   string
	channelName string
	setByUserID string
}

type fakeDiscordRegistration struct {
	registrations   []db.DiscordRegistration
	configuredIDs   map[string]struct{}
	setting         db.DiscordGuildSetting
	hasSetting      bool
	channels        []discord.Channel
	registeredCount int64
	setChannelCalls []setChannelCall
}

func (f *fakeDiscordRegistration) ListRegisteredServers(context.Context, uuid.UUID) ([]db.DiscordRegistration, error) {
	return f.registrations, nil
}

func (f *fakeDiscordRegistration) RegisterFromInteraction(context.Context, string, string, string, string) (db.DiscordRegistration, error) {
	return db.DiscordRegistration{}, nil
}

func (f *fakeDiscordRegistration) UnregisterFromGuild(context.Context, string, string) error {
	return nil
}

func (f *fakeDiscordRegistration) SetChannel(_ context.Context, guildID, guildName, channelID, channelName, setByUserID string) (db.DiscordGuildSetting, error) {
	f.setChannelCalls = append(f.setChannelCalls, setChannelCall{
		guildID:     guildID,
		guildName:   guildName,
		channelID:   channelID,
		channelName: channelName,
		setByUserID: setByUserID,
	})
	return db.DiscordGuildSetting{GuildID: guildID, ReportChannelID: channelID, ChannelName: channelName, SetByUserID: setByUserID}, nil
}

func (f *fakeDiscordRegistration) ListConfiguredGuildIDs(context.Context) (map[string]struct{}, error) {
	return f.configuredIDs, nil
}

func (f *fakeDiscordRegistration) GetChannelSettings(context.Context, string) (db.DiscordGuildSetting, bool, error) {
	return f.setting, f.hasSetting, nil
}

func (f *fakeDiscordRegistration) ListGuildTextChannels(context.Context, string) ([]discord.Channel, error) {
	return f.channels, nil
}

func (f *fakeDiscordRegistration) CountRegisteredUsers(context.Context, string) (int64, error) {
	return f.registeredCount, nil
}

func newProfileRealTemplateRenderer(t *testing.T) *handlers.TemplateRenderer {
	t.Helper()
	base, err := template.ParseFS(web.FS, "templates/base.html")
	if err != nil {
		t.Fatalf("parse base template: %v", err)
	}
	if _, err := base.ParseFS(web.FS, "templates/partials/*.html"); err != nil {
		t.Fatalf("parse partials: %v", err)
	}
	return handlers.NewTemplateRenderer(base, web.FS, "templates")
}

func serveProfileRequest(t *testing.T, user db.User, handler http.HandlerFunc, method, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	body := strings.NewReader("")
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	tp, err := auth.IssueAccessToken(user.ID, user.Email, testJWTSecret, 15*time.Minute)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: "access_token", Value: tp.AccessToken})

	mux := http.NewServeMux()
	mux.Handle(method+" /dashboard/servers/{guildID}/channel", middleware.Authenticate(&fakeProfileUsers{user: user}, testJWTSecret, false)(http.HandlerFunc(handler)))
	mux.Handle(method+" /dashboard/servers/{guildID}", middleware.Authenticate(&fakeProfileUsers{user: user}, testJWTSecret, false)(http.HandlerFunc(handler)))

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

func TestGuildPageRendersChannelSelectForManager(t *testing.T) {
	user := db.User{ID: uuid.New(), Email: "manager@example.com", SetupProgress: 5}
	guildID := "guild-1"
	discordReg := &fakeDiscordRegistration{
		setting:         db.DiscordGuildSetting{GuildID: guildID, ReportChannelID: "chan-2", ChannelName: "training"},
		hasSetting:      true,
		channels:        []discord.Channel{{ID: "chan-1", Name: "general"}, {ID: "chan-2", Name: "training"}},
		registeredCount: 7,
	}
	oauthSvc := &fakeProfileOAuth{
		memberships: []app.GuildMembership{{GuildID: guildID, GuildName: "Test Guild", IsAdmin: true}},
		identity:    db.OauthIdentity{ProviderUserID: "discord-user-1", ProviderUsername: pgtype.Text{String: "manager", Valid: true}},
	}
	h := handlers.NewProfileHandler(&fakeProfileUsers{user: user}, oauthSvc, discordReg, "", newProfileRealTemplateRenderer(t), false)

	rr := serveProfileRequest(t, user, h.GuildPage, http.MethodGet, "/dashboard/servers/"+guildID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	bodyBytes, _ := io.ReadAll(rr.Result().Body)
	body := string(bodyBytes)
	for _, want := range []string{
		`hx-post="/dashboard/servers/guild-1/channel"`,
		`<select name="channel_id"`,
		`<option value="chan-1"`,
		`#general`,
		`<option value="chan-2" selected`,
		`#training`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("rendered dashboard missing %q in:\n%s", want, body)
		}
	}
}

func TestSetGuildChannelAuthorizationAndValidation(t *testing.T) {
	user := db.User{ID: uuid.New(), Email: "manager@example.com", SetupProgress: 5}
	guildID := "guild-1"

	t.Run("non-manager is rejected and does not call SetChannel", func(t *testing.T) {
		discordReg := &fakeDiscordRegistration{channels: []discord.Channel{{ID: "chan-1", Name: "general"}}}
		oauthSvc := &fakeProfileOAuth{
			memberships: []app.GuildMembership{{GuildID: guildID, GuildName: "Test Guild", IsAdmin: false}},
			identity:    db.OauthIdentity{ProviderUserID: "discord-user-1"},
		}
		h := handlers.NewProfileHandler(&fakeProfileUsers{user: user}, oauthSvc, discordReg, "", newTestRenderer(t), false)

		rr := serveProfileRequest(t, user, h.SetGuildChannel, http.MethodPost, "/dashboard/servers/"+guildID+"/channel", url.Values{"channel_id": {"chan-1"}})
		if rr.Code != http.StatusBadRequest && rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want bad request/error partial", rr.Code)
		}
		if len(discordReg.setChannelCalls) != 0 {
			t.Fatalf("SetChannel calls = %d, want 0", len(discordReg.setChannelCalls))
		}
	})

	t.Run("non-member is rejected and does not call SetChannel", func(t *testing.T) {
		discordReg := &fakeDiscordRegistration{channels: []discord.Channel{{ID: "chan-1", Name: "general"}}}
		oauthSvc := &fakeProfileOAuth{
			memberships: []app.GuildMembership{{GuildID: "other-guild", GuildName: "Other Guild", IsAdmin: true}},
			identity:    db.OauthIdentity{ProviderUserID: "discord-user-1"},
		}
		h := handlers.NewProfileHandler(&fakeProfileUsers{user: user}, oauthSvc, discordReg, "", newTestRenderer(t), false)

		rr := serveProfileRequest(t, user, h.SetGuildChannel, http.MethodPost, "/dashboard/servers/"+guildID+"/channel", url.Values{"channel_id": {"chan-1"}})
		if rr.Code != http.StatusBadRequest && rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want bad request/error partial", rr.Code)
		}
		if len(discordReg.setChannelCalls) != 0 {
			t.Fatalf("SetChannel calls = %d, want 0", len(discordReg.setChannelCalls))
		}
	})

	t.Run("manager with valid channel calls SetChannel", func(t *testing.T) {
		discordReg := &fakeDiscordRegistration{channels: []discord.Channel{{ID: "chan-1", Name: "general"}, {ID: "chan-2", Name: "training"}}}
		oauthSvc := &fakeProfileOAuth{
			memberships: []app.GuildMembership{{GuildID: guildID, GuildName: "Test Guild", IsAdmin: true}},
			identity:    db.OauthIdentity{ProviderUserID: "discord-user-1"},
		}
		h := handlers.NewProfileHandler(&fakeProfileUsers{user: user}, oauthSvc, discordReg, "", newTestRenderer(t), false)

		rr := serveProfileRequest(t, user, h.SetGuildChannel, http.MethodPost, "/dashboard/servers/"+guildID+"/channel", url.Values{"channel_id": {"chan-2"}})
		if rr.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303", rr.Code)
		}
		if got := rr.Header().Get("Location"); got != "/dashboard/servers/"+guildID {
			t.Fatalf("Location = %q, want dashboard guild redirect", got)
		}
		if len(discordReg.setChannelCalls) != 1 {
			t.Fatalf("SetChannel calls = %d, want 1", len(discordReg.setChannelCalls))
		}
		want := setChannelCall{guildID: guildID, guildName: "Test Guild", channelID: "chan-2", channelName: "training", setByUserID: "discord-user-1"}
		if discordReg.setChannelCalls[0] != want {
			t.Fatalf("SetChannel call = %+v, want %+v", discordReg.setChannelCalls[0], want)
		}
	})

	t.Run("empty channel is rejected", func(t *testing.T) {
		discordReg := &fakeDiscordRegistration{channels: []discord.Channel{{ID: "chan-1", Name: "general"}}}
		oauthSvc := &fakeProfileOAuth{
			memberships: []app.GuildMembership{{GuildID: guildID, GuildName: "Test Guild", IsAdmin: true}},
			identity:    db.OauthIdentity{ProviderUserID: "discord-user-1"},
		}
		h := handlers.NewProfileHandler(&fakeProfileUsers{user: user}, oauthSvc, discordReg, "", newTestRenderer(t), false)

		rr := serveProfileRequest(t, user, h.SetGuildChannel, http.MethodPost, "/dashboard/servers/"+guildID+"/channel", url.Values{"channel_id": {""}})
		if rr.Code != http.StatusBadRequest && rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want bad request/error partial", rr.Code)
		}
		if len(discordReg.setChannelCalls) != 0 {
			t.Fatalf("SetChannel calls = %d, want 0", len(discordReg.setChannelCalls))
		}
	})

	t.Run("unknown channel is rejected", func(t *testing.T) {
		discordReg := &fakeDiscordRegistration{channels: []discord.Channel{{ID: "chan-1", Name: "general"}}}
		oauthSvc := &fakeProfileOAuth{
			memberships: []app.GuildMembership{{GuildID: guildID, GuildName: "Test Guild", IsAdmin: true}},
			identity:    db.OauthIdentity{ProviderUserID: "discord-user-1"},
		}
		h := handlers.NewProfileHandler(&fakeProfileUsers{user: user}, oauthSvc, discordReg, "", newTestRenderer(t), false)

		rr := serveProfileRequest(t, user, h.SetGuildChannel, http.MethodPost, "/dashboard/servers/"+guildID+"/channel", url.Values{"channel_id": {"chan-2"}})
		if rr.Code != http.StatusBadRequest && rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want bad request/error partial", rr.Code)
		}
		if len(discordReg.setChannelCalls) != 0 {
			t.Fatalf("SetChannel calls = %d, want 0", len(discordReg.setChannelCalls))
		}
	})
}
