//go:build integration

package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/rowbot/internal/app"
	"github.com/softsrv/rowbot/internal/auth"
	"github.com/softsrv/rowbot/internal/db"
)

func rowExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) bool {
	t.Helper()
	var found bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS("+query+")", args...).Scan(&found); err != nil {
		t.Fatalf("rowExists query %q: %v", query, err)
	}
	return found
}

func TestIntegration_UserService_GetByID(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	svc := app.NewUserService(db.New(pool))

	seeded := seedUser(t, pool)

	got, err := svc.GetByID(ctx, seeded.ID)
	if err != nil {
		t.Fatalf("GetByID existing user: %v", err)
	}
	if got.ID != seeded.ID {
		t.Errorf("GetByID ID = %v, want %v", got.ID, seeded.ID)
	}
	if got.Email != seeded.Email {
		t.Errorf("GetByID Email = %v, want %v", got.Email, seeded.Email)
	}

	// Unknown user id returns ErrUserNotFound
	unknownID := newID(t)
	_, err = svc.GetByID(ctx, unknownID)
	if !errors.Is(err, app.ErrUserNotFound) {
		t.Fatalf("GetByID unknown user error = %v, want %v", err, app.ErrUserNotFound)
	}

	// Confirm directly from DB that unknown user indeed does not exist
	if rowExists(t, ctx, pool, "SELECT 1 FROM users WHERE id = $1", unknownID) {
		t.Fatalf("unknown user %v unexpectedly exists in users table", unknownID)
	}
}

func TestIntegration_UserService_ListSessions(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	svc := app.NewUserService(db.New(pool))

	user := seedUser(t, pool)
	otherUser := seedUser(t, pool)

	// Seed refresh tokens for user
	rawActive1 := mustIssueRefreshToken(t, pool, user.ID)
	rawActive2 := mustIssueRefreshToken(t, pool, user.ID)
	_ = mustIssueRefreshToken(t, pool, otherUser.ID) // should not be listed for user

	// Seed tokens to be updated into revoked and expired states
	rawRevoked := mustIssueRefreshToken(t, pool, user.ID)
	rawExpired := mustIssueRefreshToken(t, pool, user.ID)

	hashRevoked := auth.HashToken(rawRevoked)
	hashExpired := auth.HashToken(rawExpired)

	// Revoke one token by its specific hash
	if _, err := pool.Exec(ctx, `
		UPDATE refresh_tokens
		SET revoked_at = NOW()
		WHERE token_hash = $1`, hashRevoked); err != nil {
		t.Fatalf("update revoked_at: %v", err)
	}

	// Expire one token by its specific hash
	if _, err := pool.Exec(ctx, `
		UPDATE refresh_tokens
		SET expires_at = NOW() - INTERVAL '1 hour'
		WHERE token_hash = $1`, hashExpired); err != nil {
		t.Fatalf("update expires_at: %v", err)
	}

	sessions, err := svc.ListSessions(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}

	// Exactly 2 active sessions should be returned
	if len(sessions) != 2 {
		t.Fatalf("ListSessions len = %d, want 2", len(sessions))
	}

	hashActive1 := auth.HashToken(rawActive1)
	hashActive2 := auth.HashToken(rawActive2)
	foundHashes := make(map[string]bool)

	now := time.Now()
	for _, sess := range sessions {
		foundHashes[sess.TokenHash] = true
		if sess.UserID != user.ID {
			t.Errorf("ListSessions returned session for wrong user: got %v, want %v", sess.UserID, user.ID)
		}
		if sess.RevokedAt.Valid {
			t.Errorf("ListSessions returned revoked session: %v", sess.ID)
		}
		if !sess.ExpiresAt.Valid || sess.ExpiresAt.Time.Before(now) {
			t.Errorf("ListSessions returned expired session: %v", sess.ID)
		}

		// Directly verify in table that this session is active
		var revokedAt *time.Time
		var expiresAt time.Time
		if err := pool.QueryRow(ctx, "SELECT revoked_at, expires_at FROM refresh_tokens WHERE id = $1", sess.ID).Scan(&revokedAt, &expiresAt); err != nil {
			t.Fatalf("query refresh_token %v: %v", sess.ID, err)
		}
		if revokedAt != nil {
			t.Errorf("direct DB check: token %v has revoked_at set: %v", sess.ID, revokedAt)
		}
		if expiresAt.Before(now) {
			t.Errorf("direct DB check: token %v has expires_at in past: %v", sess.ID, expiresAt)
		}
	}

	if !foundHashes[hashActive1] || !foundHashes[hashActive2] {
		t.Errorf("ListSessions did not return expected active token hashes; got %v", foundHashes)
	}
}

func TestIntegration_UserService_RevokeSession(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	svc := app.NewUserService(db.New(pool))

	userA := seedUser(t, pool)
	userB := seedUser(t, pool)

	rawA := mustIssueRefreshToken(t, pool, userA.ID)
	rawB := mustIssueRefreshToken(t, pool, userB.ID)

	hashA := auth.HashToken(rawA)
	hashB := auth.HashToken(rawB)

	// Fetch token ids by their specific hashes
	var tokenIDA uuid.UUID
	if err := pool.QueryRow(ctx, "SELECT id FROM refresh_tokens WHERE token_hash = $1", hashA).Scan(&tokenIDA); err != nil {
		t.Fatalf("query token id A: %v", err)
	}

	var tokenIDB uuid.UUID
	if err := pool.QueryRow(ctx, "SELECT id FROM refresh_tokens WHERE token_hash = $1", hashB).Scan(&tokenIDB); err != nil {
		t.Fatalf("query token id B: %v", err)
	}

	// 1. Unknown token returns ErrTokenNotFound
	unknownTokenID := newID(t)
	_, err := svc.RevokeSession(ctx, userA.ID, unknownTokenID)
	if !errors.Is(err, app.ErrTokenNotFound) {
		t.Fatalf("RevokeSession unknown token error = %v, want %v", err, app.ErrTokenNotFound)
	}

	// 2. User A attempting to revoke User B's token returns ErrForbidden
	_, err = svc.RevokeSession(ctx, userA.ID, tokenIDB)
	if !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("RevokeSession forbidden error = %v, want %v", err, app.ErrForbidden)
	}

	// Verify User B's token was not revoked in the database
	var revokedAtB *time.Time
	if err := pool.QueryRow(ctx, "SELECT revoked_at FROM refresh_tokens WHERE id = $1", tokenIDB).Scan(&revokedAtB); err != nil {
		t.Fatalf("query token B revoked_at: %v", err)
	}
	if revokedAtB != nil {
		t.Fatalf("token B was revoked after unauthorized call: %v", revokedAtB)
	}

	// 3. User A revokes their own token -> succeeds
	revokedToken, err := svc.RevokeSession(ctx, userA.ID, tokenIDA)
	if err != nil {
		t.Fatalf("RevokeSession owner: %v", err)
	}
	if revokedToken.ID != tokenIDA {
		t.Errorf("revoked token ID = %v, want %v", revokedToken.ID, tokenIDA)
	}

	// Directly verify DB state: revoked_at is now set
	var revokedAtA *time.Time
	if err := pool.QueryRow(ctx, "SELECT revoked_at FROM refresh_tokens WHERE id = $1", tokenIDA).Scan(&revokedAtA); err != nil {
		t.Fatalf("query token A revoked_at: %v", err)
	}
	if revokedAtA == nil {
		t.Fatalf("direct DB check: token A revoked_at is still NULL after RevokeSession")
	}
}

func TestIntegration_UserService_DeleteAccount(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	svc := app.NewUserService(db.New(pool))
	enc := testEncrypter(t)

	user := seedUser(t, pool)

	// Seed associated refresh tokens, oauth identity, and oauth token
	_ = mustIssueRefreshToken(t, pool, user.ID)
	identity := seedOAuthIdentity(t, pool, user.ID, "discord", "")
	_ = seedOAuthToken(t, pool, enc, identity.ID, "access-token", "refresh-token", time.Now().Add(time.Hour))

	// Ensure rows exist before deletion
	if !rowExists(t, ctx, pool, "SELECT 1 FROM users WHERE id = $1", user.ID) {
		t.Fatalf("expected user %v to exist before delete", user.ID)
	}
	if !rowExists(t, ctx, pool, "SELECT 1 FROM refresh_tokens WHERE user_id = $1", user.ID) {
		t.Fatalf("expected refresh_tokens for user %v to exist before delete", user.ID)
	}
	if !rowExists(t, ctx, pool, "SELECT 1 FROM oauth_identities WHERE user_id = $1", user.ID) {
		t.Fatalf("expected oauth_identities for user %v to exist before delete", user.ID)
	}
	if !rowExists(t, ctx, pool, "SELECT 1 FROM oauth_tokens WHERE oauth_identity_id = $1", identity.ID) {
		t.Fatalf("expected oauth_tokens for identity %v to exist before delete", identity.ID)
	}

	// DeleteAccount
	if err := svc.DeleteAccount(ctx, user.ID); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}

	// Assert database state: user removed
	var dummy uuid.UUID
	err := pool.QueryRow(ctx, "SELECT id FROM users WHERE id = $1", user.ID).Scan(&dummy)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("query user after DeleteAccount: expected ErrNoRows, got %v", err)
	}

	// Assert cascade: refresh_tokens removed
	if rowExists(t, ctx, pool, "SELECT 1 FROM refresh_tokens WHERE user_id = $1", user.ID) {
		t.Fatalf("refresh_tokens for user %v still exist after DeleteAccount cascade", user.ID)
	}

	// Assert cascade: oauth_identities removed
	if rowExists(t, ctx, pool, "SELECT 1 FROM oauth_identities WHERE user_id = $1", user.ID) {
		t.Fatalf("oauth_identities for user %v still exist after DeleteAccount cascade", user.ID)
	}

	// Assert cascade: oauth_tokens removed
	if rowExists(t, ctx, pool, "SELECT 1 FROM oauth_tokens WHERE oauth_identity_id = $1", identity.ID) {
		t.Fatalf("oauth_tokens for identity %v still exist after DeleteAccount cascade", identity.ID)
	}
}

func TestIntegration_UserService_SetSetupProgress(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	svc := app.NewUserService(db.New(pool))

	user := seedUser(t, pool)

	// Default setup progress is 1
	got, err := svc.GetByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetByID initial: %v", err)
	}
	if got.SetupProgress != 1 {
		t.Fatalf("initial setup_progress = %d, want 1", got.SetupProgress)
	}

	// Update to progress step 3
	if err := svc.SetSetupProgress(ctx, user.ID, 3); err != nil {
		t.Fatalf("SetSetupProgress 3: %v", err)
	}

	// Fresh read via UserService
	updated, err := svc.GetByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetByID after SetSetupProgress: %v", err)
	}
	if updated.SetupProgress != 3 {
		t.Errorf("GetByID SetupProgress = %d, want 3", updated.SetupProgress)
	}

	// Directly query DB table to verify persistence
	var dbProgress int32
	if err := pool.QueryRow(ctx, "SELECT setup_progress FROM users WHERE id = $1", user.ID).Scan(&dbProgress); err != nil {
		t.Fatalf("direct DB query setup_progress: %v", err)
	}
	if dbProgress != 3 {
		t.Errorf("direct DB check setup_progress = %d, want 3", dbProgress)
	}

	// Update to step 5 (completed wizard)
	if err := svc.SetSetupProgress(ctx, user.ID, 5); err != nil {
		t.Fatalf("SetSetupProgress 5: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT setup_progress FROM users WHERE id = $1", user.ID).Scan(&dbProgress); err != nil {
		t.Fatalf("direct DB query setup_progress: %v", err)
	}
	if dbProgress != 5 {
		t.Errorf("direct DB check setup_progress = %d, want 5", dbProgress)
	}
}
