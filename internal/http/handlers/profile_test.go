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

type removeChannelSettingsCall struct {
	guildID string
}

type fakeDiscordRegistration struct {
	registrations              []db.DiscordRegistration
	configuredIDs              map[string]struct{}
	setting                    db.DiscordGuildSetting
	hasSetting                 bool
	channels                   []discord.Channel
	registeredCount            int64
	setChannelCalls            []setChannelCall
	removeChannelSettingsCalls []removeChannelSettingsCall
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

func (f *fakeDiscordRegistration) RemoveChannelSettings(_ context.Context, guildID string) error {
	f.removeChannelSettingsCalls = append(f.removeChannelSettingsCalls, removeChannelSettingsCall{guildID: guildID})
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
	return serveProfileRequestWithHeaders(t, user, handler, method, target, form, nil)
}

func serveProfileRequestWithHeaders(t *testing.T, user db.User, handler http.HandlerFunc, method, target string, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body := strings.NewReader("")
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
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
		`hx-include="#channel-id"`,
		`<input id="channel-id" name="channel_id" type="hidden" value="chan-2" data-channel-name="#training"`,
		`<input id="channel-picker-filter" type="search" class="input input-bordered w-full js-channel-picker-filter"`,
		`placeholder="Search channels…"`,
		`<ul id="channel-picker-options" class="menu hidden mt-1 max-h-72 w-full flex-nowrap gap-1 overflow-y-auto rounded-box bg-base-200 p-2"`,
		`data-channel-id="chan-1" data-channel-name="#general" aria-selected="false"`,
		`#general`,
		`data-channel-id="chan-2" data-channel-name="#training" aria-selected="true"`,
		`js-channel-picker-option justify-start py-2 active`,
		`#training`,
		`js-channel-picker-empty hidden mt-1 rounded-box bg-base-200 p-3 text-sm italic text-base-content/60">No channels found`,
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

	t.Run("manager with valid channel htmx request returns partial without redirect", func(t *testing.T) {
		discordReg := &fakeDiscordRegistration{channels: []discord.Channel{{ID: "chan-1", Name: "general"}, {ID: "chan-2", Name: "training"}}, registeredCount: 7}
		oauthSvc := &fakeProfileOAuth{
			memberships: []app.GuildMembership{{GuildID: guildID, GuildName: "Test Guild", IsAdmin: true}},
			identity:    db.OauthIdentity{ProviderUserID: "discord-user-1"},
		}
		h := handlers.NewProfileHandler(&fakeProfileUsers{user: user}, oauthSvc, discordReg, "", newProfileRealTemplateRenderer(t), false)

		rr := serveProfileRequestWithHeaders(t, user, h.SetGuildChannel, http.MethodPost, "/dashboard/servers/"+guildID+"/channel", url.Values{"channel_id": {"chan-2"}}, map[string]string{"HX-Request": "true"})
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		if got := rr.Header().Get("HX-Redirect"); got != "" {
			t.Fatalf("HX-Redirect = %q, want empty", got)
		}
		bodyBytes, _ := io.ReadAll(rr.Result().Body)
		body := string(bodyBytes)
		for _, want := range []string{`id="channel-region"`, `#training`, `hx-post="/dashboard/servers/guild-1/channel"`} {
			if !strings.Contains(body, want) {
				t.Fatalf("partial missing %q in:\n%s", want, body)
			}
		}
		if len(discordReg.setChannelCalls) != 1 {
			t.Fatalf("SetChannel calls = %d, want 1", len(discordReg.setChannelCalls))
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

func TestGuildPageRendersChannelRemoveControlForManager(t *testing.T) {
	user := db.User{ID: uuid.New(), Email: "manager@example.com", SetupProgress: 5}
	guildID := "guild-1"

	render := func(t *testing.T, hasSetting, isAdmin bool) string {
		t.Helper()
		discordReg := &fakeDiscordRegistration{
			setting:    db.DiscordGuildSetting{GuildID: guildID, ReportChannelID: "chan-2", ChannelName: "training"},
			hasSetting: hasSetting,
			channels:   []discord.Channel{{ID: "chan-1", Name: "general"}, {ID: "chan-2", Name: "training"}},
		}
		oauthSvc := &fakeProfileOAuth{memberships: []app.GuildMembership{{GuildID: guildID, GuildName: "Test Guild", IsAdmin: isAdmin}}}
		h := handlers.NewProfileHandler(&fakeProfileUsers{user: user}, oauthSvc, discordReg, "", newProfileRealTemplateRenderer(t), false)
		rr := serveProfileRequest(t, user, h.GuildPage, http.MethodGet, "/dashboard/servers/"+guildID, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		bodyBytes, _ := io.ReadAll(rr.Result().Body)
		return string(bodyBytes)
	}

	adminConfigured := render(t, true, true)
	if !strings.Contains(adminConfigured, `js-open-channel-remove-confirm`) || !strings.Contains(adminConfigured, `>Remove</button>`) {
		t.Fatalf("admin configured page missing remove control in:\n%s", adminConfigured)
	}
	if !strings.Contains(adminConfigured, `id="channel-remove-modal" class="modal"`) {
		t.Fatalf("admin configured page missing remove confirmation modal in:\n%s", adminConfigured)
	}

	adminNotConfigured := render(t, false, true)
	if strings.Contains(adminNotConfigured, `js-open-channel-remove-confirm`) {
		t.Fatalf("not-configured page rendered remove control in:\n%s", adminNotConfigured)
	}
	if !strings.Contains(adminNotConfigured, `>Set channel</button>`) {
		t.Fatalf("not-configured page missing existing Set channel control in:\n%s", adminNotConfigured)
	}

	nonAdminConfigured := render(t, true, false)
	if strings.Contains(nonAdminConfigured, `js-open-channel-remove-confirm`) {
		t.Fatalf("non-admin page rendered remove control in:\n%s", nonAdminConfigured)
	}
}

func TestChannelRemoveModalHTMXWiring(t *testing.T) {
	user := db.User{ID: uuid.New(), Email: "manager@example.com", SetupProgress: 5}
	guildID := "guild-1"
	discordReg := &fakeDiscordRegistration{
		setting:    db.DiscordGuildSetting{GuildID: guildID, ReportChannelID: "chan-2", ChannelName: "training"},
		hasSetting: true,
		channels:   []discord.Channel{{ID: "chan-1", Name: "general"}, {ID: "chan-2", Name: "training"}},
	}
	oauthSvc := &fakeProfileOAuth{memberships: []app.GuildMembership{{GuildID: guildID, GuildName: "Test Guild", IsAdmin: true}}}
	h := handlers.NewProfileHandler(&fakeProfileUsers{user: user}, oauthSvc, discordReg, "", newProfileRealTemplateRenderer(t), false)
	rr := serveProfileRequest(t, user, h.GuildPage, http.MethodGet, "/dashboard/servers/"+guildID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	bodyBytes, _ := io.ReadAll(rr.Result().Body)
	body := string(bodyBytes)

	removeIdx := strings.Index(body, `js-open-channel-remove-confirm`)
	if removeIdx == -1 {
		t.Fatalf("remove button missing in:\n%s", body)
	}
	buttonStart := strings.LastIndex(body[:removeIdx], `<button`)
	buttonEndRel := strings.Index(body[removeIdx:], `</button>`)
	if buttonStart == -1 || buttonEndRel == -1 {
		t.Fatalf("could not isolate remove button in:\n%s", body)
	}
	removeButton := body[buttonStart : removeIdx+buttonEndRel]
	if strings.Contains(removeButton, `hx-delete`) {
		t.Fatalf("remove button carries hx-delete: %s", removeButton)
	}
	for _, want := range []string{
		`id="channel-remove-modal" class="modal"`,
		`hx-delete="/dashboard/servers/guild-1/channel"`,
		`hx-target="#channel-region"`,
		`hx-swap="outerHTML"`,
		`hx-disable="this"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("remove modal missing %q in:\n%s", want, body)
		}
	}
}

func TestRemoveGuildChannelAuthorizationAndHTMXResponse(t *testing.T) {
	user := db.User{ID: uuid.New(), Email: "manager@example.com", SetupProgress: 5}
	guildID := "guild-1"

	t.Run("non-manager is rejected and does not remove settings", func(t *testing.T) {
		discordReg := &fakeDiscordRegistration{channels: []discord.Channel{{ID: "chan-1", Name: "general"}}}
		oauthSvc := &fakeProfileOAuth{memberships: []app.GuildMembership{{GuildID: guildID, GuildName: "Test Guild", IsAdmin: false}}}
		h := handlers.NewProfileHandler(&fakeProfileUsers{user: user}, oauthSvc, discordReg, "", newTestRenderer(t), false)
		rr := serveProfileRequest(t, user, h.RemoveGuildChannel, http.MethodDelete, "/dashboard/servers/"+guildID+"/channel", nil)
		if rr.Code != http.StatusBadRequest && rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want bad request/error partial", rr.Code)
		}
		if len(discordReg.removeChannelSettingsCalls) != 0 {
			t.Fatalf("RemoveChannelSettings calls = %d, want 0", len(discordReg.removeChannelSettingsCalls))
		}
	})

	t.Run("non-member is rejected and does not remove settings", func(t *testing.T) {
		discordReg := &fakeDiscordRegistration{channels: []discord.Channel{{ID: "chan-1", Name: "general"}}}
		oauthSvc := &fakeProfileOAuth{memberships: []app.GuildMembership{{GuildID: "other-guild", GuildName: "Other Guild", IsAdmin: true}}}
		h := handlers.NewProfileHandler(&fakeProfileUsers{user: user}, oauthSvc, discordReg, "", newTestRenderer(t), false)
		rr := serveProfileRequest(t, user, h.RemoveGuildChannel, http.MethodDelete, "/dashboard/servers/"+guildID+"/channel", nil)
		if rr.Code != http.StatusBadRequest && rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want bad request/error partial", rr.Code)
		}
		if len(discordReg.removeChannelSettingsCalls) != 0 {
			t.Fatalf("RemoveChannelSettings calls = %d, want 0", len(discordReg.removeChannelSettingsCalls))
		}
	})

	t.Run("manager removes settings and redirects for non-htmx", func(t *testing.T) {
		discordReg := &fakeDiscordRegistration{channels: []discord.Channel{{ID: "chan-1", Name: "general"}}}
		oauthSvc := &fakeProfileOAuth{memberships: []app.GuildMembership{{GuildID: guildID, GuildName: "Test Guild", IsAdmin: true}}}
		h := handlers.NewProfileHandler(&fakeProfileUsers{user: user}, oauthSvc, discordReg, "", newTestRenderer(t), false)
		rr := serveProfileRequest(t, user, h.RemoveGuildChannel, http.MethodDelete, "/dashboard/servers/"+guildID+"/channel", nil)
		if rr.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303", rr.Code)
		}
		if got := rr.Header().Get("Location"); got != "/dashboard/servers/"+guildID {
			t.Fatalf("Location = %q, want dashboard guild redirect", got)
		}
		if len(discordReg.removeChannelSettingsCalls) != 1 || discordReg.removeChannelSettingsCalls[0].guildID != guildID {
			t.Fatalf("RemoveChannelSettings calls = %+v, want guild %s", discordReg.removeChannelSettingsCalls, guildID)
		}
	})

	t.Run("manager htmx request removes settings and returns not-configured partial", func(t *testing.T) {
		discordReg := &fakeDiscordRegistration{channels: []discord.Channel{{ID: "chan-1", Name: "general"}}, registeredCount: 7}
		oauthSvc := &fakeProfileOAuth{memberships: []app.GuildMembership{{GuildID: guildID, GuildName: "Test Guild", IsAdmin: true}}}
		h := handlers.NewProfileHandler(&fakeProfileUsers{user: user}, oauthSvc, discordReg, "", newProfileRealTemplateRenderer(t), false)
		rr := serveProfileRequestWithHeaders(t, user, h.RemoveGuildChannel, http.MethodDelete, "/dashboard/servers/"+guildID+"/channel", nil, map[string]string{"HX-Request": "true"})
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		bodyBytes, _ := io.ReadAll(rr.Result().Body)
		body := string(bodyBytes)
		for _, want := range []string{`id="channel-region"`, `Not set`, `>Set channel</button>`, `hx-post="/dashboard/servers/guild-1/channel"`} {
			if !strings.Contains(body, want) {
				t.Fatalf("partial missing %q in:\n%s", want, body)
			}
		}
		if strings.Contains(body, `js-open-channel-remove-confirm`) {
			t.Fatalf("not-configured partial rendered remove control in:\n%s", body)
		}
		if len(discordReg.removeChannelSettingsCalls) != 1 || discordReg.removeChannelSettingsCalls[0].guildID != guildID {
			t.Fatalf("RemoveChannelSettings calls = %+v, want guild %s", discordReg.removeChannelSettingsCalls, guildID)
		}
	})
}
