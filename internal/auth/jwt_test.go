package auth_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nevup/trade-journal/internal/auth"
)

const testSecret = "97791d4db2aa5f689c3cc39356ce35762f0a73aa70923039d8ef72a2840a1b02"

func TestIssueAndVerify(t *testing.T) {
	v := auth.NewVerifier([]byte(testSecret))
	tok, err := auth.IssueToken([]byte(testSecret), "f412f236-4edc-47a2-8f54-8763a6ed2ce8", "Alex Mercer")
	require.NoError(t, err)

	claims, err := v.Parse(tok)
	require.NoError(t, err)
	require.Equal(t, "f412f236-4edc-47a2-8f54-8763a6ed2ce8", claims.UserID.String())
	require.Equal(t, "trader", claims.Role)
	require.Equal(t, "Alex Mercer", claims.Name)
}

func TestRejectsBadSignature(t *testing.T) {
	v := auth.NewVerifier([]byte(testSecret))
	tok, err := auth.IssueToken([]byte("a-different-secret"), "f412f236-4edc-47a2-8f54-8763a6ed2ce8", "")
	require.NoError(t, err)

	_, err = v.Parse(tok)
	require.ErrorIs(t, err, auth.ErrInvalidToken)
}

func TestRejectsMalformed(t *testing.T) {
	v := auth.NewVerifier([]byte(testSecret))
	for _, bad := range []string{"", "not-a-jwt", "a.b.c"} {
		_, err := v.Parse(bad)
		require.ErrorIs(t, err, auth.ErrInvalidToken, "input=%q", bad)
	}
}
