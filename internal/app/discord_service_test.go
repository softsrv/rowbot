package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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
