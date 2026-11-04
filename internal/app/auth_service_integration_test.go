//go:build integration

package app_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/rowbot/internal/app"
	"github.com/softsrv/rowbot/internal/auth"
	"github.com/softsrv/rowbot/internal/db"
)

// testDB connects to the database pointed to by TEST_DATABASE_URL.
// Run integration tests through make, which guards, migrates, then tests:
//
//	TEST_DATABASE_URL=postgres://.../<name>_test make test-integration
//
// (`make test-db-up` starts a disposable one and prints its URL.)
// A missing URL, an unreachable database or an unmigrated schema fails the
// test with a hint: an integration test never skips.
func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL is not set: run `make test-integration` with it pointing at a disposable *_test database (`make test-db-up` starts one)")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("database unreachable: invalid TEST_DATABASE_URL (`make test-db-up` prints a valid one): %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("database unreachable at TEST_DATABASE_URL (is it running? `make test-db-up`): %v", err)
	}
	var missing bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('public.users') IS NULL").Scan(&missing); err != nil {
		t.Fatalf("schema check failed (start a test database with `make test-db-up`, then run `make test-integration`): %v", err)
	}
	if missing {
		t.Fatal("schema not migrated: table users is missing; run `make test-integration` (it applies migrations via test-db-migrate; `make test-db-up` starts a database)")
	}
	return pool
}

// mustCleanupDelete registers a t.Cleanup that runs one DELETE and reports its
// error: a failed cleanup would leave rows behind for later tests.
func mustCleanupDelete(t *testing.T, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), query, args...); err != nil {
			t.Errorf("cleanup %q: %v", query, err)
		}
	})
}

func testAuthService(t *testing.T, pool *pgxpool.Pool) *app.AuthService {
	t.Helper()
	q := db.New(pool)
	return app.NewAuthService(q, pool, app.AuthServiceConfig{
		JWTSecret:     "integration-test-secret-32-bytes!!!",
		AccessExpiry:  15 * time.Minute,
		RefreshExpiry: 720 * time.Hour,
		AppBaseURL:    "http://localhost:8080",
	})
}

// testUser creates a user directly via db.Queries.CreateUser (mirroring how
// every real account is provisioned — Discord OAuth is the sole
// account-creation path) so Refresh/Logout can be exercised without
// AuthService.Register/Login, which no longer exist. The email is made unique
// with the user id, and a t.Cleanup deletes the user (cascading its tokens).
func testUser(t *testing.T, pool *pgxpool.Pool, emailPrefix string) db.User {
	t.Helper()
	q := db.New(pool)
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}
	user, err := q.CreateUser(context.Background(), db.CreateUserParams{
		ID:    id,
		Email: fmt.Sprintf("%s-%s@example.com", emailPrefix, id),
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	mustCleanupDelete(t, pool, "DELETE FROM users WHERE id = $1", user.ID)
	return user
}

func TestIntegration_Refresh(t *testing.T) {
	pool := testDB(t)
	svc := testAuthService(t, pool)
	ctx := context.Background()

	user := testUser(t, pool, "refresh")

	result, err := svc.Refresh(ctx, mustIssueRefreshToken(t, pool, user.ID), app.DeviceMeta{})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// Refresh returns a new access token but the same refresh token.
	result2, err := svc.Refresh(ctx, result.RefreshToken, app.DeviceMeta{})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if result2.RefreshToken != result.RefreshToken {
		t.Error("refresh token must not change (no rotation)")
	}
	if result2.AccessToken == result.AccessToken {
		t.Error("access token must be newly issued on each refresh")
	}

	// Revoked token must be rejected.
	if err := svc.Logout(ctx, result.RefreshToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	_, err = svc.Refresh(ctx, result.RefreshToken, app.DeviceMeta{})
	if err != app.ErrTokenRevoked {
		t.Errorf("expected ErrTokenRevoked after logout, got %v", err)
	}
}

// mustIssueRefreshToken inserts a refresh token row directly and returns its
// raw (unhashed) value, letting tests exercise Refresh/Logout without going
// through a full OAuth login.
func mustIssueRefreshToken(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) string {
	t.Helper()
	q := db.New(pool)
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}
	raw, hashed, err := auth.GenerateRefreshToken()
	if err != nil {
		t.Fatalf("generate refresh token: %v", err)
	}
	_, err = q.InsertRefreshToken(context.Background(), db.InsertRefreshTokenParams{
		ID:        id,
		UserID:    userID,
		TokenHash: hashed,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(720 * time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertRefreshToken: %v", err)
	}
	return raw
}
