package auth

import (
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// IssueToken signs an HS256 JWT for the given subject. Used by the test suite
// (and could be used by a /login endpoint if we ever add one). 24h validity
// matches the hackathon spec.
func IssueToken(secret []byte, sub, name string) (string, error) {
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":  sub,
		"iat":  now.Unix(),
		"exp":  now.Add(24 * time.Hour).Unix(),
		"role": "trader",
		"name": name,
	})
	return tok.SignedString(secret)
}
