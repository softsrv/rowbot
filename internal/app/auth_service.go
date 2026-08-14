package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/softsrv/rowbot/internal/auth"
	"github.com/softsrv/rowbot/internal/db"
)

// DeviceMeta holds metadata captured from the HTTP request.
type DeviceMeta struct {
	DeviceName string
	IPAddress  *netip.Addr
	UserAgent  string
}

// TokenResult is returned after a successful login or token refresh.
type TokenResult struct {
	AccessToken        string
	AccessTokenExpiry  time.Time
	RefreshToken       string
	RefreshTokenExpiry time.Time
	EmailVerified      bool
}

// pgxBeginner is satisfied by *pgxpool.Pool.
type pgxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// AuthServiceConfig holds the configuration for AuthService.
// Using a config struct instead of positional parameters makes call sites
// self-documenting and prevents accidental argument transposition.
type AuthServiceConfig struct {
	JWTSecret     string
	AccessExpiry  time.Duration
	RefreshExpiry time.Duration
	AppBaseURL    string
}

// AuthService handles all authentication business logic. Account creation and
// login happen exclusively via Discord OAuth (see OAuthService); AuthService
// retains only the generic session lifecycle (issuing, refreshing, and
// revoking tokens) shared by every sign-in path.
type AuthService struct {
	q    *db.Queries
	pool pgxBeginner
	cfg  AuthServiceConfig
}

// NewAuthService constructs an AuthService with all dependencies injected.
func NewAuthService(q *db.Queries, pool pgxBeginner, cfg AuthServiceConfig) *AuthService {
	return &AuthService{
		q:    q,
		pool: pool,
		cfg:  cfg,
	}
}

// Logout revokes the given raw refresh token.
func (s *AuthService) Logout(ctx context.Context, rawRefreshToken string) error {
	hash := auth.HashToken(rawRefreshToken)
	rt, err := s.q.GetRefreshTokenByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("get refresh token: %w", err)
	}
	return s.q.RevokeRefreshToken(ctx, rt.ID)
}

// Refresh validates a raw refresh token, issues a new access token, and returns
// the same refresh token (no rotation). The refresh token's last_used_at and
// device metadata are updated in place.
func (s *AuthService) Refresh(ctx context.Context, rawRefreshToken string, meta DeviceMeta) (TokenResult, error) {
	hash := auth.HashToken(rawRefreshToken)

	rt, err := s.q.GetRefreshTokenByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TokenResult{}, ErrTokenNotFound
		}
		return TokenResult{}, fmt.Errorf("get refresh token: %w", err)
	}

	if rt.RevokedAt.Valid {
		return TokenResult{}, ErrTokenRevoked
	}

	if rt.ExpiresAt.Time.Before(time.Now()) {
		return TokenResult{}, ErrTokenExpired
	}

	user, err := s.q.GetUserByID(ctx, rt.UserID)
	if err != nil {
		return TokenResult{}, fmt.Errorf("get user: %w", err)
	}

	if err := s.q.UpdateRefreshTokenLastUsed(ctx, db.UpdateRefreshTokenLastUsedParams{
		ID:         rt.ID,
		DeviceName: pgtype.Text{String: meta.DeviceName, Valid: meta.DeviceName != ""},
		IpAddress:  meta.IPAddress,
		UserAgent:  pgtype.Text{String: meta.UserAgent, Valid: meta.UserAgent != ""},
	}); err != nil {
		slog.Error("update refresh token last used", "token_id", rt.ID, "error", err)
	}

	tp, err := auth.IssueAccessToken(user.ID, user.Email, s.cfg.JWTSecret, s.cfg.AccessExpiry)
	if err != nil {
		return TokenResult{}, fmt.Errorf("issue access token: %w", err)
	}

	return TokenResult{
		AccessToken:        tp.AccessToken,
		AccessTokenExpiry:  tp.ExpiresAt,
		RefreshToken:       rawRefreshToken,
		RefreshTokenExpiry: rt.ExpiresAt.Time,
	}, nil
}

// ── private helpers ───────────────────────────────────────────────────────────

func (s *AuthService) issueTokenPair(
	ctx context.Context,
	userID uuid.UUID,
	userEmail string,
	meta DeviceMeta,
) (TokenResult, error) {
	tp, err := auth.IssueAccessToken(userID, userEmail, s.cfg.JWTSecret, s.cfg.AccessExpiry)
	if err != nil {
		return TokenResult{}, fmt.Errorf("issue access token: %w", err)
	}

	rawRefresh, hashedRefresh, err := auth.GenerateRefreshToken()
	if err != nil {
		return TokenResult{}, fmt.Errorf("generate refresh token: %w", err)
	}

	newID, err := uuid.NewV7()
	if err != nil {
		return TokenResult{}, fmt.Errorf("generate token id: %w", err)
	}
	expiresAt := time.Now().Add(s.cfg.RefreshExpiry)

	_, err = s.q.InsertRefreshToken(ctx, db.InsertRefreshTokenParams{
		ID:         newID,
		UserID:     userID,
		TokenHash:  hashedRefresh,
		DeviceName: pgtype.Text{String: meta.DeviceName, Valid: meta.DeviceName != ""},
		IpAddress:  meta.IPAddress,
		UserAgent:  pgtype.Text{String: meta.UserAgent, Valid: meta.UserAgent != ""},
		ExpiresAt:  pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		return TokenResult{}, fmt.Errorf("insert refresh token: %w", err)
	}

	return TokenResult{
		AccessToken:        tp.AccessToken,
		AccessTokenExpiry:  tp.ExpiresAt,
		RefreshToken:       rawRefresh,
		RefreshTokenExpiry: expiresAt,
	}, nil
}
