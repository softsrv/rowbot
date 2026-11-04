package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Claims are the JWT payload fields.
type Claims struct {
	Email string `json:"email"`
	jwt.RegisteredClaims
}

// TokenPair holds a newly-issued access token and its signed string form.
type TokenPair struct {
	AccessToken string
	ExpiresAt   time.Time
}

// tokenIssuer is used as both the issuer and audience claim on every JWT
// this app issues and validates. Defined once so the issuing and validating
// sides can never drift out of sync.
const tokenIssuer = "RowBot"

var (
	ErrTokenExpired = errors.New("token expired")
	ErrTokenInvalid = errors.New("token invalid")
)

// IssueAccessToken creates and signs a new JWT access token.
func IssueAccessToken(userID uuid.UUID, email, secret string, expiry time.Duration) (TokenPair, error) {
	now := time.Now()
	exp := now.Add(expiry)

	claims := Claims{
		Email: email,
		RegisteredClaims: jwt.RegisteredClaims{
			// A unique jti makes every issued token distinct, even two issued
			// within the same second (iat/exp have one-second resolution).
			ID:        uuid.NewString(),
			Subject:   userID.String(),
			Issuer:    tokenIssuer,
			Audience:  jwt.ClaimStrings{tokenIssuer},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		return TokenPair{}, fmt.Errorf("sign token: %w", err)
	}

	return TokenPair{AccessToken: signed, ExpiresAt: exp}, nil
}

// ValidateAccessToken parses and validates a JWT, returning its claims.
func ValidateAccessToken(tokenStr, secret string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	}, jwt.WithIssuer(tokenIssuer), jwt.WithAudience(tokenIssuer))

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, ErrTokenInvalid
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, ErrTokenInvalid
	}

	return claims, nil
}
