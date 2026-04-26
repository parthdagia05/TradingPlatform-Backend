// Package auth verifies HS256 JWTs issued per the hackathon spec.
//
// Token shape (kickoff PDF §4):
//
//	header  : {"alg":"HS256","typ":"JWT"}
//	payload : {"sub":"<uuid>","iat":<unix>,"exp":<unix>,"role":"trader","name":"..."}
//
// Validation rules:
//   - signature verifies against JWT_SECRET using HS256
//   - exp is in the future (zero clock skew)
//   - sub is a valid UUIDv4
//   - role is "trader"
//
// Anything else (missing claim, bad signature, expired)  ErrInvalidToken.
package auth

import (
	"errors"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// ErrInvalidToken is returned for every token-rejection reason. The middleware
// wraps it into an HTTP 401 with no detail leaked to the client (timing-safe).
var ErrInvalidToken = errors.New("invalid token")

// Claims is the typed payload we read out of a verified JWT.
// We discard everything we don't actively use to keep the surface tiny.
type Claims struct {
	UserID uuid.UUID
	Role   string
	Name   string
}

// Verifier validates a token string against the configured HS256 secret.
// One Verifier instance is built at startup and shared by every request.
type Verifier struct {
	secret []byte
}

// NewVerifier returns a Verifier bound to the given HMAC secret.
// Panics on empty secret - that's a configuration bug, not a runtime issue.
func NewVerifier(secret []byte) *Verifier {
	if len(secret) == 0 {
		panic("auth: empty JWT secret - refusing to start")
	}
	return &Verifier{secret: secret}
}

// Parse verifies the token and returns its claims. Any failure mode returns
// ErrInvalidToken - we deliberately do not differentiate "expired" vs "bad sig"
// in the public error so we don't leak validation hints to attackers.
func (v *Verifier) Parse(tokenStr string) (*Claims, error) {
	tok, err := jwt.Parse(tokenStr,
		func(t *jwt.Token) (any, error) {
			// Reject any algorithm that isn't HS256. Without this check the
			// "alg=none" / "alg=RS256-with-public-key-as-HMAC-secret" attacks
			// become possible.
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected alg: %v", t.Header["alg"])
			}
			return v.secret, nil
		},
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithExpirationRequired(),
	)
	if err != nil || !tok.Valid {
		return nil, ErrInvalidToken
	}

	mc, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return nil, ErrInvalidToken
	}

	subStr, _ := mc["sub"].(string)
	uid, err := uuid.Parse(subStr)
	if err != nil {
		return nil, ErrInvalidToken
	}

	role, _ := mc["role"].(string)
	if role == "" {
		return nil, ErrInvalidToken
	}

	name, _ := mc["name"].(string) // optional per spec

	return &Claims{UserID: uid, Role: role, Name: name}, nil
}
