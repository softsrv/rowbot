package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestGetGuildChannels(t *testing.T) {
	const (
		testGuildID = "123456789"
		testToken   = "my-bot-token"
		wantPath    = "/guilds/" + testGuildID + "/channels"
		wantAuthHdr = "Bot " + testToken
	)

	t.Run("returns only guild text channels in Discord order", func(t *testing.T) {
		var (
			gotMethod string
			gotPath   string
			gotAuth   string
		)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")

			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode([]struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Type int    `json:"type"`
			}{
				{ID: "1", Name: "general", Type: channelTypeGuildText},
				{ID: "2", Name: "voice", Type: 2},
				{ID: "3", Name: "announcements", Type: 5},
				{ID: "4", Name: "thread", Type: 11},
				{ID: "5", Name: "training", Type: channelTypeGuildText},
			}); err != nil {
				t.Errorf("encode response: %v", err)
			}
		}))
		defer srv.Close()

		origBase := discordAPIBase
		discordAPIBase = srv.URL
		defer func() { discordAPIBase = origBase }()

		got, err := GetGuildChannels(context.Background(), srv.Client(), testToken, testGuildID)
		if err != nil {
			t.Fatalf("GetGuildChannels: %v", err)
		}

		if gotMethod != http.MethodGet {
			t.Errorf("method = %q, want %q", gotMethod, http.MethodGet)
		}
		if gotPath != wantPath {
			t.Errorf("path = %q, want %q", gotPath, wantPath)
		}
		if gotAuth != wantAuthHdr {
			t.Errorf("Authorization = %q, want %q", gotAuth, wantAuthHdr)
		}

		want := []Channel{
			{ID: "1", Name: "general"},
			{ID: "5", Name: "training"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("channels = %+v, want %+v", got, want)
		}
	})

	t.Run("non-2xx response returns error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"401: Unauthorized"}`))
		}))
		defer srv.Close()

		origBase := discordAPIBase
		discordAPIBase = srv.URL
		defer func() { discordAPIBase = origBase }()

		_, err := GetGuildChannels(context.Background(), srv.Client(), testToken, testGuildID)
		if err == nil {
			t.Fatal("expected error for 401 response, got nil")
		}
	})
}
