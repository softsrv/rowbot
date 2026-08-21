-- name: UpsertGuildSettings :one
INSERT INTO discord_guild_settings (id, guild_id, report_channel_id, channel_name, set_by_user_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
ON CONFLICT (guild_id) DO UPDATE
    SET report_channel_id = EXCLUDED.report_channel_id,
        channel_name      = EXCLUDED.channel_name,
        set_by_user_id    = EXCLUDED.set_by_user_id,
        updated_at        = NOW()
RETURNING *;

-- name: GetGuildSettings :one
SELECT * FROM discord_guild_settings WHERE guild_id = $1 LIMIT 1;

-- name: DeleteGuildSettings :exec
DELETE FROM discord_guild_settings WHERE guild_id = $1;

-- name: ListConfiguredGuildIDs :many
SELECT guild_id FROM discord_guild_settings;
