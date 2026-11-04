//go:build integration

package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/rowbot/internal/app"
	"github.com/softsrv/rowbot/internal/db"
)

// exists reports whether a row with the given id is present in table.
func exists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, id uuid.UUID) bool {
	t.Helper()
	var found bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM "+table+" WHERE id = $1)", id,
	).Scan(&found); err != nil {
		t.Fatalf("exists query %s %s: %v", table, id, err)
	}
	return found
}

// TestIntegration_TokenCleanup_RefreshTokens verifies the two refresh-token
// deletion conditions:
//
//   - expired more than 90 days ago        → deleted
//   - revoked more than 90 days ago        → deleted
//   - active (future expiry, not revoked)  → preserved
//   - revoked less than 90 days ago        → preserved
func TestIntegration_TokenCleanup_RefreshTokens(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	userID := seedUser(t, pool).ID

	insertRT := func(tokenHash string, expiresAt time.Time, revokedAt *time.Time) uuid.UUID {
		t.Helper()
		id, err := uuid.NewV7()
		if err != nil {
			t.Fatalf("uuid: %v", err)
		}
		if revokedAt != nil {
			if _, err := pool.Exec(ctx, `
				INSERT INTO refresh_tokens
					(id, user_id, token_hash, expires_at, created_at, last_used_at, revoked_at)
				VALUES ($1, $2, $3, $4, NOW(), NOW(), $5)`,
				id, userID, tokenHash, expiresAt, *revokedAt,
			); err != nil {
				t.Fatalf("insert refresh token: %v", err)
			}
		} else {
			if _, err := pool.Exec(ctx, `
				INSERT INTO refresh_tokens
					(id, user_id, token_hash, expires_at, created_at, last_used_at)
				VALUES ($1, $2, $3, $4, NOW(), NOW())`,
				id, userID, tokenHash, expiresAt,
			); err != nil {
				t.Fatalf("insert refresh token: %v", err)
			}
		}
		return id
	}

	now := time.Now()
	ago91 := now.Add(-91 * 24 * time.Hour)
	ago1 := now.Add(-24 * time.Hour)
	future := now.Add(30 * 24 * time.Hour)

	staleExpired := insertRT("rt-stale-expired", ago91, nil)     // expires_at > 90d ago → deleted
	staleRevoked := insertRT("rt-stale-revoked", future, &ago91) // revoked_at > 90d ago → deleted
	freshActive := insertRT("rt-fresh-active", future, nil)      // valid, not revoked → preserved
	freshRevoked := insertRT("rt-fresh-revoked", future, &ago1)  // revoked only 1d ago → preserved

	app.CleanupTokens(ctx, db.New(pool))

	if exists(t, ctx, pool, "refresh_tokens", staleExpired) {
		t.Error("expired refresh token (91d old) should have been deleted")
	}
	if exists(t, ctx, pool, "refresh_tokens", staleRevoked) {
		t.Error("revoked refresh token (91d ago) should have been deleted")
	}
	if !exists(t, ctx, pool, "refresh_tokens", freshActive) {
		t.Error("active refresh token should not have been deleted")
	}
	if !exists(t, ctx, pool, "refresh_tokens", freshRevoked) {
		t.Error("recently revoked refresh token (1d ago) should not have been deleted")
	}
}

// TestIntegration_TokenCleanup_VerificationCodes verifies the two verification-code
// deletion conditions:
//
//   - used more than 7 days ago            → deleted
//   - expired more than 1 day ago          → deleted
//   - used less than 7 days ago            → preserved
//   - pending (not yet expired, not used)  → preserved
func TestIntegration_TokenCleanup_VerificationCodes(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	userID := seedUser(t, pool).ID

	insertEVC := func(code string, expiresAt time.Time, usedAt *time.Time) uuid.UUID {
		t.Helper()
		id, err := uuid.NewV7()
		if err != nil {
			t.Fatalf("uuid: %v", err)
		}
		if usedAt != nil {
			if _, err := pool.Exec(ctx, `
				INSERT INTO email_verification_codes (id, user_id, token_hash, expires_at, created_at, used_at)
				VALUES ($1, $2, $3, $4, NOW(), $5)`,
				id, userID, code, expiresAt, *usedAt,
			); err != nil {
				t.Fatalf("insert verification code: %v", err)
			}
		} else {
			if _, err := pool.Exec(ctx, `
				INSERT INTO email_verification_codes (id, user_id, token_hash, expires_at, created_at)
				VALUES ($1, $2, $3, $4, NOW())`,
				id, userID, code, expiresAt,
			); err != nil {
				t.Fatalf("insert verification code: %v", err)
			}
		}
		return id
	}

	now := time.Now()
	ago8 := now.Add(-8 * 24 * time.Hour)
	ago6 := now.Add(-6 * 24 * time.Hour) // within 7-day used_at window → preserved
	ago2 := now.Add(-2 * 24 * time.Hour) // beyond 1-day expires_at threshold → deleted
	future := now.Add(time.Hour)

	staleUsed := insertEVC("111111", future, &ago8)  // used_at > 7d ago → deleted
	staleExpired := insertEVC("222222", ago2, nil)   // expires_at > 1d ago → deleted
	freshUsed := insertEVC("333333", future, &ago6)  // used_at only 6d ago → preserved
	freshPending := insertEVC("444444", future, nil) // not expired, not used → preserved

	app.CleanupTokens(ctx, db.New(pool))

	if exists(t, ctx, pool, "email_verification_codes", staleUsed) {
		t.Error("used verification code (8d ago) should have been deleted")
	}
	if exists(t, ctx, pool, "email_verification_codes", staleExpired) {
		t.Error("expired verification code (2d ago) should have been deleted")
	}
	if !exists(t, ctx, pool, "email_verification_codes", freshUsed) {
		t.Error("recently used verification code (6d ago) should not have been deleted")
	}
	if !exists(t, ctx, pool, "email_verification_codes", freshPending) {
		t.Error("pending verification code should not have been deleted")
	}
}
