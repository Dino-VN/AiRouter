// Package authn issues and validates the credentials used by the web UI:
// bcrypt password hashes and short-lived JWT access tokens.
package authn

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"aihub/internal/model"
)

// ErrInvalidToken is returned for any token that cannot be trusted.
var ErrInvalidToken = errors.New("authn: invalid token")

// MinPasswordLength is the shortest password the API accepts.
const MinPasswordLength = 8

// HashPassword returns a bcrypt hash for a plaintext password.
func HashPassword(password string) (string, error) {
	if len(password) < MinPasswordLength {
		return "", fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

// CheckPassword reports whether a plaintext password matches a stored hash.
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// Claims is the payload of an access token.
type Claims struct {
	jwt.RegisteredClaims

	// Username is carried for logging and display without a database round trip.
	Username string `json:"username"`
	// Role gates admin-only endpoints.
	Role model.Role `json:"role"`
	// SessionID ties the access token to the refresh session that minted it, so
	// logging out invalidates both.
	SessionID string `json:"sid,omitempty"`
}

// UserID parses the subject into a UUID.
func (c *Claims) UserID() (uuid.UUID, error) {
	return uuid.Parse(c.Subject)
}

// Issuer mints and verifies access tokens.
type Issuer struct {
	secret []byte
	ttl    time.Duration
}

// NewIssuer builds an Issuer.
func NewIssuer(secret []byte, ttl time.Duration) *Issuer {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &Issuer{secret: secret, ttl: ttl}
}

// TTL exposes the access-token lifetime so handlers can report expires_in.
func (i *Issuer) TTL() time.Duration { return i.ttl }

// Issue mints an access token for a user.
func (i *Issuer) Issue(user *model.User, sessionID uuid.UUID) (string, time.Time, error) {
	now := time.Now()
	expiresAt := now.Add(i.ttl)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   user.ID.String(),
			Issuer:    "aihub",
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-30 * time.Second)),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			ID:        uuid.NewString(),
		},
		Username: user.Username,
		Role:     user.Role,
	}
	if sessionID != uuid.Nil {
		claims.SessionID = sessionID.String()
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(i.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// Verify parses and validates an access token.
func (i *Issuer) Verify(token string) (*Claims, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrInvalidToken
	}

	parsed, err := jwt.ParseWithClaims(token, &Claims{}, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method %q", t.Method.Alg())
		}
		return i.secret, nil
	}, jwt.WithIssuer("aihub"), jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	claims, ok := parsed.Claims.(*Claims)
	if !ok || !parsed.Valid {
		return nil, ErrInvalidToken
	}
	if _, err = claims.UserID(); err != nil {
		return nil, ErrInvalidToken
	}
	return claims, nil
}
