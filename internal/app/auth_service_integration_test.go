//go:build integration

package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/softsrv/rowbot/internal/app"
	"github.com/softsrv/rowbot/internal/db"
)

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

func TestIntegration_Refresh(t *testing.T) {
	pool := testDB(t)
	svc := testAuthService(t, pool)
	ctx := context.Background()

	user := seedUser(t, pool)

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
