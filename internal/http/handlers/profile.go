package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/softsrv/rowbot/internal/app"
	"github.com/softsrv/rowbot/internal/db"
	"github.com/softsrv/rowbot/internal/discord"
	"github.com/softsrv/rowbot/internal/http/middleware"
)

// setupStepFirst/setupStepLast bound the dashboard onboarding wizard's
// visible steps; setupStepDone is the terminal sentinel meaning the wizard
// is finished and must never be shown again. See db/migrations for the
// setup_progress column semantics.
//
// The four steps: 1 asks whether the user is adding RowBot themselves — its
// "Yes" link navigates the same tab to Discord's install flow and back,
// landing on OAuthHandler.DiscordBotInstallCallback, which advances progress
// to step 2 and redirects to /dashboard once the bot is actually installed
// (an earlier version opened a second tab and used a purely client-side
// toggle for this transition, which reverted on reload; that's gone now that
// the whole install round-trip happens in this same tab); 2 asks them to run
// /setchannel; 3 registers them into a server; 4 connects Concept2.
const (
	setupStepFirst = 1
	setupStepLast  = 4
	setupStepDone  = 5
)

type profileUserServicer interface {
	DeleteAccount(ctx context.Context, userID uuid.UUID) error
	SetSetupProgress(ctx context.Context, userID uuid.UUID, progress int32) error
}

// ProfileOAuthServicer is the OAuth capability subset needed by the profile page.
// Exported so router.go can pass *app.OAuthService without a direct dependency.
type ProfileOAuthServicer interface {
	IsDiscordEnabled() bool
	IsConcept2Enabled() bool
	GetDiscordIdentity(ctx context.Context, userID uuid.UUID) (db.OauthIdentity, error)
	GetConcept2Identity(ctx context.Context, userID uuid.UUID) (db.OauthIdentity, error)
	GetDiscordGuildMemberships(ctx context.Context, userID uuid.UUID) ([]app.GuildMembership, error)
}

// discordRegistrationServicer is the subset of app.DiscordService needed by
// the dashboard to list which Discord servers the user has registered in,
// perform a new registration or remove one on their behalf, and know which
// servers are actually ready to receive results (configured via
// /setchannel).
type discordRegistrationServicer interface {
	ListRegisteredServers(ctx context.Context, userID uuid.UUID) ([]db.DiscordRegistration, error)
	RegisterFromInteraction(ctx context.Context, discordUserID, discordUsername, guildID, guildName string) (db.DiscordRegistration, error)
	UnregisterFromGuild(ctx context.Context, discordUserID, guildID string) error
	RemoveChannelSettings(ctx context.Context, guildID string) error
	SetChannel(ctx context.Context, guildID, guildName, channelID, channelName, setByUserID string) (db.DiscordGuildSetting, error)
	ListConfiguredGuildIDs(ctx context.Context) (map[string]struct{}, error)
	GetChannelSettings(ctx context.Context, guildID string) (db.DiscordGuildSetting, bool, error)
	ListGuildTextChannels(ctx context.Context, guildID string) ([]discord.Channel, error)
	CountRegisteredUsers(ctx context.Context, guildID string) (int64, error)
}

// ProfileHandler groups profile management HTTP handlers.
type ProfileHandler struct {
	users                profileUserServicer
	oauth                ProfileOAuthServicer
	discordReg           discordRegistrationServicer
	discordBotInstallURL string
	renderer             *TemplateRenderer
	secure               bool
}

// NewProfileHandler constructs a ProfileHandler. oauthSvc may be nil when
// OAuth is not configured; the dashboard will hide the Connections card.
// discordBotInstallURL may be empty when Discord is not configured or the bot
// install feature is disabled. discordReg may be nil, in which case the
// dashboard treats the user as having no server registration.
func NewProfileHandler(userSvc profileUserServicer, oauthSvc ProfileOAuthServicer, discordReg discordRegistrationServicer, discordBotInstallURL string, renderer *TemplateRenderer, secure bool) *ProfileHandler {
	return &ProfileHandler{
		users:                userSvc,
		oauth:                oauthSvc,
		discordReg:           discordReg,
		discordBotInstallURL: discordBotInstallURL,
		renderer:             renderer,
		secure:               secure,
	}
}

// ProfilePage renders the user profile page: the Connections card (Concept2
// Logbook connection status) and, when Discord is configured, a Servers card
// with the "Add a server" modal — installing the bot elsewhere or
// registering into a server it's already in. That used to live in the
// dashboard's sidebar; it moved here because the vast majority of users only
// ever have one server, so dedicating dashboard chrome to server management
// year-round didn't pay for itself — see GuildPage's own server switcher for
// the (rare) multi-server case.
func (h *ProfileHandler) ProfilePage(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.UserFromContext(r.Context())
	if !ok {
		// Shouldn't happen behind authMW, but fail safe rather than panic
		// on a nil User if this handler is ever reached some other way.
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	data := map[string]any{
		"User":                 user,
		"Concept2Enabled":      false,
		"Concept2Linked":       false,
		"Concept2Username":     "",
		"DiscordEnabled":       h.oauth != nil && h.oauth.IsDiscordEnabled(),
		"DiscordBotInstallURL": h.discordBotInstallURL,
		"RegisterableGuilds":   []app.GuildMembership(nil),
	}

	if h.oauth != nil && h.oauth.IsConcept2Enabled() {
		data["Concept2Enabled"] = true
		if identity, err := h.oauth.GetConcept2Identity(r.Context(), user.ID); err == nil {
			data["Concept2Linked"] = true
			data["Concept2Username"] = identity.ProviderUsername.String
		}
	}

	gd := h.loadGuildData(r.Context(), user.ID)
	data["RegisterableGuilds"] = gd.registerableGuilds

	h.renderer.Page(w, http.StatusOK, "profile.html", data)
}

// DashboardPage renders (while setup_progress < setupStepDone) a
// one-step-at-a-time onboarding wizard keyed off the user's persisted
// setup_progress. Once setup is done it redirects to the user's first
// server's detail page instead — see the redirect below — so this only ever
// renders content itself during onboarding, or as a fallback if setup is
// done but the user somehow has no servers yet. This route is behind
// authMW, so a user is always present in context.
func (h *ProfileHandler) DashboardPage(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.UserFromContext(r.Context())
	if !ok {
		// Shouldn't happen behind authMW, but fail safe rather than panic
		// on a nil User if this handler is ever reached some other way.
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	progress := int(user.SetupProgress)
	if progress < setupStepFirst {
		progress = setupStepFirst
	}
	wizardStep := progress
	if wizardStep > setupStepLast {
		wizardStep = setupStepLast
	}
	showWizard := progress < setupStepDone

	gd := h.loadGuildData(r.Context(), user.ID)

	// Once setup is done, the wizard card no longer renders, so with no
	// server explicitly selected this page would have nothing to show. Send
	// the user straight into their first server's detail page instead, which
	// now owns its own server switcher for anyone with more than one.
	if !showWizard && len(gd.userGuilds) > 0 {
		http.Redirect(w, r, "/dashboard/servers/"+gd.userGuilds[0].GuildID, http.StatusSeeOther)
		return
	}

	data := map[string]any{
		"User":                     user,
		"DiscordBotInstallURL":     h.discordBotInstallURL,
		"WizardRegisterableGuilds": gd.wizardRegisterableGuilds,
		"Concept2Linked":           false,
		"Concept2Username":         "",
		"ShowWizard":               showWizard,
		"WizardStep":               wizardStep,
	}

	// Only the wizard's step 4 needs Concept2 status now — the Connections
	// card (Discord and Concept2 alike) moved to the profile page.
	if h.oauth != nil && h.oauth.IsConcept2Enabled() {
		if identity, err := h.oauth.GetConcept2Identity(r.Context(), user.ID); err == nil {
			data["Concept2Linked"] = true
			data["Concept2Username"] = identity.ProviderUsername.String
		}
	}

	h.renderer.Page(w, http.StatusOK, "dashboard.html", data)
}

// guildData bundles everything derived from a user's Discord guild
// memberships and server registrations, shared by DashboardPage, GuildPage,
// and ProfilePage so each renders a consistent view without re-fetching and
// re-filtering the same data a different way.
type guildData struct {
	registeredServers        []db.DiscordRegistration
	registeredGuildIDs       map[string]struct{}
	memberships              []app.GuildMembership
	registerableGuilds       []app.GuildMembership
	wizardRegisterableGuilds []app.GuildMembership
	userGuilds               []app.UserGuild
}

// loadGuildData fetches the user's registered servers and (if Discord is
// configured) their live Discord guild memberships, then derives every
// filtered view the dashboard and guild-detail pages need. Errors are logged
// and swallowed — this is supplementary dashboard info, not core to either
// page, and a partial/empty result degrades gracefully rather than breaking
// the page.
func (h *ProfileHandler) loadGuildData(ctx context.Context, userID uuid.UUID) guildData {
	var gd guildData

	if h.discordReg != nil {
		regs, err := h.discordReg.ListRegisteredServers(ctx, userID)
		if err != nil {
			slog.WarnContext(ctx, "load guild data: list discord registrations", "user_id", userID, "error", err)
		} else {
			gd.registeredServers = regs
			gd.registeredGuildIDs = make(map[string]struct{}, len(regs))
			for _, reg := range regs {
				gd.registeredGuildIDs[reg.GuildID] = struct{}{}
			}
		}
	}

	if h.oauth == nil || !h.oauth.IsDiscordEnabled() {
		return gd
	}

	memberships, err := h.oauth.GetDiscordGuildMemberships(ctx, userID)
	if err != nil {
		slog.WarnContext(ctx, "load guild data: get discord guild memberships", "user_id", userID, "error", err)
		return gd
	}
	gd.memberships = memberships

	var configuredGuildIDs map[string]struct{}
	if h.discordReg != nil {
		configuredGuildIDs, err = h.discordReg.ListConfiguredGuildIDs(ctx)
		if err != nil {
			slog.WarnContext(ctx, "load guild data: list configured guild ids", "user_id", userID, "error", err)
		}
	}

	// The profile page's "register for a server" picker only offers servers
	// that are both unregistered and actually configured (have a
	// discord_guild_settings row) — a bot-installed-but-unconfigured server
	// isn't useful yet from there. The setup wizard's step 3 picker is
	// deliberately looser: any unregistered, bot-installed server, whether or
	// not /setchannel has run yet, matching its own copy ("ask someone with
	// 'manage' permission to add RowBot") which only requires the bot to be
	// present. RegisterFromInteraction itself has never required a
	// configured channel, so this is safe either way. userGuilds, meanwhile,
	// is every guild the user currently belongs to that they either manage
	// or are registered in, regardless of configuration state — the set
	// GuildPage's server switcher offers.
	gd.registerableGuilds = make([]app.GuildMembership, 0, len(memberships))
	gd.wizardRegisterableGuilds = make([]app.GuildMembership, 0, len(memberships))
	gd.userGuilds = make([]app.UserGuild, 0, len(memberships))
	for _, g := range memberships {
		_, isRegistered := gd.registeredGuildIDs[g.GuildID]

		if !isRegistered {
			gd.wizardRegisterableGuilds = append(gd.wizardRegisterableGuilds, g)
			if _, ok := configuredGuildIDs[g.GuildID]; ok {
				gd.registerableGuilds = append(gd.registerableGuilds, g)
			}
		}

		if g.IsAdmin || isRegistered {
			gd.userGuilds = append(gd.userGuilds, app.UserGuild{
				GuildID:      g.GuildID,
				GuildName:    g.GuildName,
				IsRegistered: isRegistered,
				IsAdmin:      g.IsAdmin,
			})
		}
	}

	return gd
}

// GuildPage renders the detail page for one Discord server, with a
// dropdown server switcher in place of the card title whenever the user
// belongs to more than one (see UserGuilds) — most users only ever have
// one, so the switcher only appears when it's actually useful. guildID must
// be one the user has a legitimate relationship to — either a current
// membership (per live Discord data) or an existing registration (which
// covers the edge case of a stale registration for a server they've since
// left, so they can still see/clean it up rather than being stranded).
// Anything else redirects to /dashboard without revealing whether the guild
// exists.
func (h *ProfileHandler) GuildPage(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.UserFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	guildID := r.PathValue("guildID")
	showWizard := int(user.SetupProgress) < setupStepDone
	gd := h.loadGuildData(r.Context(), user.ID)

	var guildName string
	var isAdmin, isRegistered, found bool
	for _, g := range gd.memberships {
		if g.GuildID == guildID {
			guildName = g.GuildName
			isAdmin = g.IsAdmin
			found = true
			break
		}
	}
	if _, ok := gd.registeredGuildIDs[guildID]; ok {
		isRegistered = true
		found = true
		if guildName == "" {
			for _, reg := range gd.registeredServers {
				if reg.GuildID == guildID {
					guildName = reg.GuildName
					break
				}
			}
		}
	}
	if !found {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}

	var channelConfigured bool
	var channelName, currentChannelID string
	if h.discordReg != nil {
		if setting, ok, err := h.discordReg.GetChannelSettings(r.Context(), guildID); err != nil {
			slog.WarnContext(r.Context(), "guild page: get channel settings", "guild_id", guildID, "error", err)
		} else if ok {
			channelConfigured = true
			channelName = setting.ChannelName
			currentChannelID = setting.ReportChannelID
		}
	}

	// Only managers see this, so only bother counting and fetching channel
	// choices for them.
	var registeredCount int64
	var textChannels []discord.Channel
	if isAdmin && h.discordReg != nil {
		if count, err := h.discordReg.CountRegisteredUsers(r.Context(), guildID); err != nil {
			slog.WarnContext(r.Context(), "guild page: count registered users", "guild_id", guildID, "error", err)
		} else {
			registeredCount = count
		}
		if channels, err := h.discordReg.ListGuildTextChannels(r.Context(), guildID); err != nil {
			slog.WarnContext(r.Context(), "guild page: list text channels", "guild_id", guildID, "error", err)
		} else {
			textChannels = channels
		}
	}

	data := map[string]any{
		"User":              user,
		"UserGuilds":        gd.userGuilds,
		"CurrentGuildID":    guildID,
		"GuildID":           guildID,
		"GuildName":         guildName,
		"IsAdmin":           isAdmin,
		"IsRegistered":      isRegistered,
		"ShowWizard":        showWizard,
		"ChannelConfigured": channelConfigured,
		"ChannelName":       channelName,
		"CurrentChannelID":  currentChannelID,
		"TextChannels":      textChannels,
		"RegisteredCount":   registeredCount,
	}

	h.renderer.Page(w, http.StatusOK, "dashboard-server.html", data)
}

// UnregisterDiscordServer removes the authenticated user's registration in
// one guild, driven from that guild's detail page. A non-admin has no
// remaining reason to view the page once unregistered, so they're sent back
// to /dashboard; an admin may still be managing the server, so they land back
// on the same guild page instead.
func (h *ProfileHandler) UnregisterDiscordServer(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if h.oauth == nil || h.discordReg == nil {
		h.renderError(w, r, http.StatusBadRequest, "Discord is not configured.")
		return
	}

	guildID := r.PathValue("guildID")

	identity, err := h.oauth.GetDiscordIdentity(r.Context(), user.ID)
	if err != nil {
		slog.WarnContext(r.Context(), "unregister discord server: get discord identity", "user_id", user.ID, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "Failed to unregister. Please try again.")
		return
	}

	if err := h.discordReg.UnregisterFromGuild(r.Context(), identity.ProviderUserID, guildID); err != nil {
		slog.WarnContext(r.Context(), "unregister discord server: unregister from guild", "user_id", user.ID, "guild_id", guildID, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "Failed to unregister. Please try again.")
		return
	}

	redirectPath := "/dashboard"
	memberships, err := h.oauth.GetDiscordGuildMemberships(r.Context(), user.ID)
	if err != nil {
		slog.WarnContext(r.Context(), "unregister discord server: get discord guild memberships", "user_id", user.ID, "error", err)
	} else {
		for _, g := range memberships {
			if g.GuildID == guildID && g.IsAdmin {
				redirectPath = "/dashboard/servers/" + guildID
				break
			}
		}
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", redirectPath)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, redirectPath, http.StatusSeeOther)
}

// RemoveGuildChannel removes one guild's configured reporting channel from the
// web dashboard. The guild identifier is never trusted at face value: the
// handler re-fetches the user's Discord guild memberships to prove they manage
// this guild before deleting the settings row.
func (h *ProfileHandler) RemoveGuildChannel(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if h.oauth == nil || h.discordReg == nil {
		h.renderError(w, r, http.StatusBadRequest, "Discord is not configured.")
		return
	}

	guildID := r.PathValue("guildID")
	memberships, err := h.oauth.GetDiscordGuildMemberships(r.Context(), user.ID)
	if err != nil {
		slog.WarnContext(r.Context(), "remove guild channel: get guild memberships", "user_id", user.ID, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "Failed to remove the channel. Please try again.")
		return
	}

	var guildName string
	var isManager bool
	for _, g := range memberships {
		if g.GuildID == guildID && g.IsAdmin {
			guildName = g.GuildName
			isManager = true
			break
		}
	}
	if !isManager {
		h.renderError(w, r, http.StatusBadRequest, "That server isn't available to manage. Refresh the dashboard and try again.")
		return
	}

	channels, err := h.discordReg.ListGuildTextChannels(r.Context(), guildID)
	if err != nil {
		slog.WarnContext(r.Context(), "remove guild channel: list text channels", "guild_id", guildID, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "Failed to remove the channel. Please try again.")
		return
	}

	if err := h.discordReg.RemoveChannelSettings(r.Context(), guildID); err != nil {
		slog.WarnContext(r.Context(), "remove guild channel: remove channel settings", "user_id", user.ID, "guild_id", guildID, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "Failed to remove the channel. Please try again.")
		return
	}

	redirectPath := "/dashboard/servers/" + guildID
	if r.Header.Get("HX-Request") == "true" {
		var registeredCount int64
		if count, err := h.discordReg.CountRegisteredUsers(r.Context(), guildID); err != nil {
			slog.WarnContext(r.Context(), "remove guild channel: count registered users", "guild_id", guildID, "error", err)
		} else {
			registeredCount = count
		}
		data := map[string]any{
			"GuildID":           guildID,
			"GuildName":         guildName,
			"IsAdmin":           true,
			"ChannelConfigured": false,
			"ChannelName":       "",
			"CurrentChannelID":  "",
			"TextChannels":      channels,
			"RegisteredCount":   registeredCount,
		}
		h.renderer.Partial(w, http.StatusOK, "channel-region", data)
		return
	}
	http.Redirect(w, r, redirectPath, http.StatusSeeOther)
}

// SetGuildChannel updates one guild's reporting channel from the web dashboard.
// The submitted guild and channel identifiers are never trusted at face value:
// the handler re-fetches the user's Discord guild memberships to prove they
// manage this guild, and re-fetches Discord's text-channel list to prove the
// posted channel belongs to it before writing.
func (h *ProfileHandler) SetGuildChannel(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if h.oauth == nil || h.discordReg == nil {
		h.renderError(w, r, http.StatusBadRequest, "Discord is not configured.")
		return
	}

	if err := r.ParseForm(); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "Invalid form submission.")
		return
	}
	channelID := r.Form.Get("channel_id")
	if channelID == "" {
		h.renderError(w, r, http.StatusBadRequest, "Please choose a reporting channel.")
		return
	}

	guildID := r.PathValue("guildID")
	memberships, err := h.oauth.GetDiscordGuildMemberships(r.Context(), user.ID)
	if err != nil {
		slog.WarnContext(r.Context(), "set guild channel: get guild memberships", "user_id", user.ID, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "Failed to update the channel. Please try again.")
		return
	}

	var guildName string
	var isManager bool
	for _, g := range memberships {
		if g.GuildID == guildID && g.IsAdmin {
			guildName = g.GuildName
			isManager = true
			break
		}
	}
	if !isManager {
		h.renderError(w, r, http.StatusBadRequest, "That server isn't available to manage. Refresh the dashboard and try again.")
		return
	}

	channels, err := h.discordReg.ListGuildTextChannels(r.Context(), guildID)
	if err != nil {
		slog.WarnContext(r.Context(), "set guild channel: list text channels", "guild_id", guildID, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "Failed to update the channel. Please try again.")
		return
	}
	var channelName string
	var channelFound bool
	for _, ch := range channels {
		if ch.ID == channelID {
			channelName = ch.Name
			channelFound = true
			break
		}
	}
	if !channelFound {
		h.renderError(w, r, http.StatusBadRequest, "Please choose an available text channel.")
		return
	}

	identity, err := h.oauth.GetDiscordIdentity(r.Context(), user.ID)
	if err != nil {
		slog.WarnContext(r.Context(), "set guild channel: get discord identity", "user_id", user.ID, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "Failed to update the channel. Please try again.")
		return
	}

	if _, err := h.discordReg.SetChannel(r.Context(), guildID, guildName, channelID, channelName, identity.ProviderUserID); err != nil {
		slog.WarnContext(r.Context(), "set guild channel: set channel", "user_id", user.ID, "guild_id", guildID, "channel_id", channelID, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "Failed to update the channel. Please try again.")
		return
	}

	redirectPath := "/dashboard/servers/" + guildID
	if r.Header.Get("HX-Request") == "true" {
		var registeredCount int64
		if count, err := h.discordReg.CountRegisteredUsers(r.Context(), guildID); err != nil {
			slog.WarnContext(r.Context(), "set guild channel: count registered users", "guild_id", guildID, "error", err)
		} else {
			registeredCount = count
		}
		data := map[string]any{
			"GuildID":           guildID,
			"IsAdmin":           true,
			"ChannelConfigured": true,
			"ChannelName":       channelName,
			"CurrentChannelID":  channelID,
			"TextChannels":      channels,
			"RegisteredCount":   registeredCount,
		}
		h.renderer.Partial(w, http.StatusOK, "channel-region", data)
		return
	}
	http.Redirect(w, r, redirectPath, http.StatusSeeOther)
}

// SetupNext advances the authenticated user's dashboard wizard by one step,
// clamped at setupStepDone (the terminal "never show again" sentinel). Used
// by step 2's and step 4's "Done" buttons — step 1 advances via
// DiscordBotInstallCallback instead (once the bot install round-trip
// actually completes) and step 3 advances via RegisterDiscordServer, since
// each of those actions *is* the advance.
func (h *ProfileHandler) SetupNext(w http.ResponseWriter, r *http.Request) {
	h.stepSetupProgress(w, r, 1, setupStepDone)
}

// SetupSkipToRegister jumps the wizard from step 1 straight to step 3,
// skipping step 2 (running /setchannel). Used by step 1's "No" button — a
// user who isn't the one adding RowBot to a server has no reason to be asked
// to run a command in a server they don't manage.
func (h *ProfileHandler) SetupSkipToRegister(w http.ResponseWriter, r *http.Request) {
	h.stepSetupProgress(w, r, 2, setupStepLast)
}

// SetupPrevious steps the wizard back two steps, from step 3 to step 1. Only
// step 3's empty-list dead end offers this in the UI, when there's nothing
// to register into and no forward action available. It mirrors
// SetupSkipToRegister's forward +2 jump rather than a plain -1: step 3 is
// reachable either via step 2 or by skipping it from step 1's "No", and
// landing back on step 2 would wrongly ask a "No" user to run /setchannel in
// a server they said they don't manage. Going back two lands everyone on
// step 1 instead, where the branch choice is made fresh.
func (h *ProfileHandler) SetupPrevious(w http.ResponseWriter, r *http.Request) {
	h.stepSetupProgress(w, r, -2, setupStepFirst)
}

// stepSetupProgress applies delta to the user's setup_progress, clamps the
// result against limit (an upper bound for delta>0, a lower bound for
// delta<0), persists it, and redirects back to /dashboard.
func (h *ProfileHandler) stepSetupProgress(w http.ResponseWriter, r *http.Request, delta, limit int) {
	user, ok := middleware.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	next := int(user.SetupProgress) + delta
	if delta > 0 && next > limit {
		next = limit
	}
	if delta < 0 && next < limit {
		next = limit
	}

	if err := h.users.SetSetupProgress(r.Context(), user.ID, int32(next)); err != nil {
		slog.ErrorContext(r.Context(), "set setup progress", "user_id", user.ID, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "Failed to update setup progress. Please try again.")
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/dashboard")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// DeleteAccount permanently deletes the authenticated user's account.
func (h *ProfileHandler) DeleteAccount(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if err := h.users.DeleteAccount(r.Context(), user.ID); err != nil {
		slog.ErrorContext(r.Context(), "delete account", "user_id", user.ID, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "Failed to delete account. Please try again.")
		return
	}

	clearAuthCookies(w, h.secure)
	w.Header().Set("HX-Redirect", "/dashboard")
	w.WriteHeader(http.StatusOK)
}

// RegisterDiscordServer registers the authenticated user in a Discord server
// they belong to, driven from the dashboard's step-3 picker as a website-side
// alternative to running /register in Discord itself. The submitted guild_id
// is never trusted at face value — it must appear in a freshly-fetched list
// of the user's own bot-installed guild memberships, so a tampered form value
// can't register someone into an arbitrary guild they don't belong to.
func (h *ProfileHandler) RegisterDiscordServer(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if h.oauth == nil || h.discordReg == nil {
		h.renderError(w, r, http.StatusBadRequest, "Discord is not configured.")
		return
	}

	if err := r.ParseForm(); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "Invalid form submission.")
		return
	}
	guildID := r.Form.Get("guild_id")
	if guildID == "" {
		h.renderError(w, r, http.StatusBadRequest, "Please choose a server.")
		return
	}

	memberships, err := h.oauth.GetDiscordGuildMemberships(r.Context(), user.ID)
	if err != nil {
		slog.WarnContext(r.Context(), "register discord server: get guild memberships", "user_id", user.ID, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "Failed to register. Please try again.")
		return
	}

	var guildName string
	var found bool
	for _, g := range memberships {
		if g.GuildID == guildID {
			guildName, found = g.GuildName, true
			break
		}
	}
	if !found {
		h.renderError(w, r, http.StatusBadRequest, "That server isn't available to register in. Refresh the dashboard and try again.")
		return
	}

	identity, err := h.oauth.GetDiscordIdentity(r.Context(), user.ID)
	if err != nil {
		slog.WarnContext(r.Context(), "register discord server: get discord identity", "user_id", user.ID, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "Failed to register. Please try again.")
		return
	}

	if _, err := h.discordReg.RegisterFromInteraction(r.Context(), identity.ProviderUserID, identity.ProviderUsername.String, guildID, guildName); err != nil {
		slog.WarnContext(r.Context(), "register discord server: register from interaction", "user_id", user.ID, "guild_id", guildID, "error", err)
		if errors.Is(err, app.ErrGuildFull) {
			h.renderError(w, r, http.StatusBadRequest, "This server is full and can't accept new registrations. Please contact a server manager.")
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "Failed to register. Please try again.")
		return
	}

	// The wizard's step 3 has no separate "Next" button — a successful
	// registration is itself the advance to step 4. Only bump from exactly 3;
	// a user re-registering into a second server later (post-wizard) must not
	// resurrect the wizard by rewinding their already-advanced progress.
	if user.SetupProgress == 3 {
		if err := h.users.SetSetupProgress(r.Context(), user.ID, 4); err != nil {
			slog.WarnContext(r.Context(), "register discord server: advance setup progress", "user_id", user.ID, "error", err)
		}
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/dashboard")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (h *ProfileHandler) renderError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if r.Header.Get("HX-Request") == "true" {
		status = http.StatusOK
	}
	h.renderer.Partial(w, status, "partials/error.html", map[string]any{"Error": msg})
}
