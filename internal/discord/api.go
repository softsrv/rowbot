package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const channelTypeGuildText = 0

// Channel is a Discord guild channel as returned by GET /guilds/{id}/channels.
type Channel struct {
	ID   string
	Name string
}

// GetGuildChannels fetches guildID's channels from Discord (GET
// /guilds/{guildID}/channels) and returns only plain text channels
// (Discord channel type 0, GUILD_TEXT) — announcement channels, voice,
// categories, and threads are excluded. If client is nil,
// http.DefaultClient is used. A non-2xx response is returned as an error.
func GetGuildChannels(ctx context.Context, client *http.Client, botToken, guildID string) ([]Channel, error) {
	if client == nil {
		client = http.DefaultClient
	}
	url := fmt.Sprintf("%s/guilds/%s/channels", discordAPIBase, guildID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+botToken)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discord api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("discord api responded %d: %s", resp.StatusCode, bytes.TrimSpace(bodyBytes))
	}
	var apiChannels []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type int    `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiChannels); err != nil {
		return nil, fmt.Errorf("decode channels: %w", err)
	}
	channels := make([]Channel, 0, len(apiChannels))
	for _, apiChannel := range apiChannels {
		if apiChannel.Type != channelTypeGuildText {
			continue
		}
		channels = append(channels, Channel{ID: apiChannel.ID, Name: apiChannel.Name})
	}
	return channels, nil
}

// GetGuildName fetches the name of a Discord guild by its ID. If client is nil,
// http.DefaultClient is used. A non-2xx response from Discord is returned as an error.
func GetGuildName(ctx context.Context, client *http.Client, botToken, guildID string) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	url := fmt.Sprintf("%s/guilds/%s", discordAPIBase, guildID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+botToken)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("discord api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("discord api responded %d: %s", resp.StatusCode, bytes.TrimSpace(bodyBytes))
	}
	var guild struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&guild); err != nil {
		return "", fmt.Errorf("decode guild: %w", err)
	}
	return guild.Name, nil
}
