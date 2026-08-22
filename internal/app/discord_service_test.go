package app_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/softsrv/rowbot/internal/app"
	"github.com/softsrv/rowbot/internal/db"
)

type discordServiceExecRecorder struct {
	queries []string
	args    [][]interface{}
	err     error
}

func (r *discordServiceExecRecorder) Exec(_ context.Context, query string, args ...interface{}) (pgconn.CommandTag, error) {
	r.queries = append(r.queries, query)
	r.args = append(r.args, args)
	if r.err != nil {
		return pgconn.CommandTag{}, r.err
	}
	return pgconn.NewCommandTag("DELETE 1"), nil
}

func (r *discordServiceExecRecorder) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	panic("Query should not be called")
}

func (r *discordServiceExecRecorder) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	panic("QueryRow should not be called")
}

type discordRegistrationQueryRecorder struct {
	queries []string
	args    [][]interface{}

	existingRegistration *db.DiscordRegistration
	count                int64
	countErr             error
	oauthErr             error
	upsertCalls          int
	countCalls           int
}

func (r *discordRegistrationQueryRecorder) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	panic("Exec should not be called")
}

func (r *discordRegistrationQueryRecorder) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	panic("Query should not be called")
}

func (r *discordRegistrationQueryRecorder) QueryRow(_ context.Context, query string, args ...interface{}) pgx.Row {
	r.queries = append(r.queries, query)
	r.args = append(r.args, args)

	switch {
	case strings.Contains(query, "FROM oauth_identities"):
		if r.oauthErr != nil {
			return discordServiceRow{err: r.oauthErr}
		}
		return discordServiceRow{values: []any{uuid.New(), uuid.New(), "discord", args[1].(string), pgtype.Text{String: "linked-user", Valid: true}, pgtype.Timestamptz{}, pgtype.Timestamptz{}}}
	case strings.Contains(query, "INSERT INTO discord_guilds"):
		return discordServiceRow{values: []any{args[0].(uuid.UUID), args[1].(string), args[2].(string), pgtype.Timestamptz{}, pgtype.Timestamptz{}}}
	case strings.Contains(query, "FROM discord_registrations") && strings.Contains(query, "LIMIT 1"):
		if r.existingRegistration == nil {
			return discordServiceRow{err: pgx.ErrNoRows}
		}
		return discordRegistrationRow(*r.existingRegistration)
	case strings.Contains(query, "SELECT COUNT(*) FROM discord_registrations"):
		r.countCalls++
		if r.countErr != nil {
			return discordServiceRow{err: r.countErr}
		}
		return discordServiceRow{values: []any{r.count}}
	case strings.Contains(query, "INSERT INTO discord_registrations"):
		r.upsertCalls++
		return discordRegistrationRow(db.DiscordRegistration{
			ID:              args[0].(uuid.UUID),
			DiscordUserID:   args[1].(string),
			DiscordUsername: args[2].(string),
			GuildID:         args[3].(string),
			GuildName:       args[4].(string),
		})
	default:
		return discordServiceRow{err: fmt.Errorf("unexpected query: %s", query)}
	}
}

type discordServiceRow struct {
	values []any
	err    error
}

func (r discordServiceRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return fmt.Errorf("scan destination count = %d, want %d", len(dest), len(r.values))
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *uuid.UUID:
			*d = r.values[i].(uuid.UUID)
		case *string:
			*d = r.values[i].(string)
		case *pgtype.UUID:
			*d = r.values[i].(pgtype.UUID)
		case *pgtype.Text:
			*d = r.values[i].(pgtype.Text)
		case *pgtype.Timestamptz:
			*d = r.values[i].(pgtype.Timestamptz)
		case *int64:
			*d = r.values[i].(int64)
		default:
			return fmt.Errorf("unsupported scan destination %T", dest[i])
		}
	}
	return nil
}

func discordRegistrationRow(reg db.DiscordRegistration) discordServiceRow {
	return discordServiceRow{values: []any{
		reg.ID,
		reg.DiscordUserID,
		reg.DiscordUsername,
		reg.GuildID,
		reg.GuildName,
		reg.UserID,
		reg.CreatedAt,
		reg.UpdatedAt,
	}}
}

func TestDiscordServiceRegisterFromInteractionRegistrationCap(t *testing.T) {
	ctx := context.Background()

	t.Run("net-new registration at cap returns ErrGuildFull without upsert", func(t *testing.T) {
		recorder := &discordRegistrationQueryRecorder{count: app.MaxRegistrationsPerGuild, oauthErr: pgx.ErrNoRows}
		svc := app.NewDiscordService(db.New(recorder), "", nil)

		_, err := svc.RegisterFromInteraction(ctx, "discord-user-1", "rower", "guild-1", "Guild One")
		if !errors.Is(err, app.ErrGuildFull) {
			t.Fatalf("RegisterFromInteraction error = %v, want ErrGuildFull", err)
		}
		if recorder.upsertCalls != 0 {
			t.Fatalf("UpsertDiscordRegistration calls = %d, want 0", recorder.upsertCalls)
		}
		if recorder.countCalls != 1 {
			t.Fatalf("CountDiscordRegistrationsByGuild calls = %d, want 1", recorder.countCalls)
		}
	})

	t.Run("net-new registration below cap upserts", func(t *testing.T) {
		recorder := &discordRegistrationQueryRecorder{count: app.MaxRegistrationsPerGuild - 1, oauthErr: pgx.ErrNoRows}
		svc := app.NewDiscordService(db.New(recorder), "", nil)

		reg, err := svc.RegisterFromInteraction(ctx, "discord-user-2", "rower", "guild-1", "Guild One")
		if err != nil {
			t.Fatalf("RegisterFromInteraction: %v", err)
		}
		if recorder.upsertCalls != 1 {
			t.Fatalf("UpsertDiscordRegistration calls = %d, want 1", recorder.upsertCalls)
		}
		if recorder.countCalls != 1 {
			t.Fatalf("CountDiscordRegistrationsByGuild calls = %d, want 1", recorder.countCalls)
		}
		if reg.DiscordUserID != "discord-user-2" || reg.GuildID != "guild-1" {
			t.Fatalf("registration = %+v, want discord-user-2 in guild-1", reg)
		}
	})

	t.Run("existing registration skips count and upserts even at cap", func(t *testing.T) {
		existing := db.DiscordRegistration{ID: uuid.New(), DiscordUserID: "discord-user-3", DiscordUsername: "old", GuildID: "guild-1", GuildName: "Guild One"}
		recorder := &discordRegistrationQueryRecorder{existingRegistration: &existing, count: app.MaxRegistrationsPerGuild, oauthErr: pgx.ErrNoRows}
		svc := app.NewDiscordService(db.New(recorder), "", nil)

		reg, err := svc.RegisterFromInteraction(ctx, "discord-user-3", "new", "guild-1", "Guild One")
		if err != nil {
			t.Fatalf("RegisterFromInteraction: %v", err)
		}
		if recorder.countCalls != 0 {
			t.Fatalf("CountDiscordRegistrationsByGuild calls = %d, want 0", recorder.countCalls)
		}
		if recorder.upsertCalls != 1 {
			t.Fatalf("UpsertDiscordRegistration calls = %d, want 1", recorder.upsertCalls)
		}
		if reg.DiscordUsername != "new" {
			t.Fatalf("registration username = %q, want updated username", reg.DiscordUsername)
		}
	})
}

func TestDiscordServiceRemoveChannelSettings(t *testing.T) {
	ctx := context.Background()

	t.Run("existing row deleted", func(t *testing.T) {
		recorder := &discordServiceExecRecorder{}
		svc := app.NewDiscordService(db.New(recorder), "", nil)

		if err := svc.RemoveChannelSettings(ctx, "guild-1"); err != nil {
			t.Fatalf("RemoveChannelSettings: %v", err)
		}
		if len(recorder.queries) != 1 {
			t.Fatalf("Exec calls = %d, want 1", len(recorder.queries))
		}
		if got := recorder.args[0][0]; got != "guild-1" {
			t.Fatalf("DeleteGuildSettings arg = %v, want guild-1", got)
		}
	})

	t.Run("missing row is no-op", func(t *testing.T) {
		recorder := &discordServiceExecRecorder{}
		svc := app.NewDiscordService(db.New(recorder), "", nil)

		if err := svc.RemoveChannelSettings(ctx, "guild-absent"); err != nil {
			t.Fatalf("RemoveChannelSettings: %v", err)
		}
	})

	t.Run("delete error is wrapped", func(t *testing.T) {
		wantErr := errors.New("delete failed")
		recorder := &discordServiceExecRecorder{err: wantErr}
		svc := app.NewDiscordService(db.New(recorder), "", nil)

		err := svc.RemoveChannelSettings(ctx, "guild-1")
		if !errors.Is(err, wantErr) {
			t.Fatalf("RemoveChannelSettings error = %v, want wrapped %v", err, wantErr)
		}
	})
}
