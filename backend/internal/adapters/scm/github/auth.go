package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"strings"
)

// TokenSource supplies GitHub bearer tokens. It is intentionally tiny so tests
// can inject a static token and production can use env/gh fallback.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

type StaticTokenSource string

func (s StaticTokenSource) Token(context.Context) (string, error) {
	if strings.TrimSpace(string(s)) == "" {
		return "", ErrNoToken
	}
	return strings.TrimSpace(string(s)), nil
}

var ErrNoToken = errors.New("github scm: no token")

type EnvTokenSource struct {
	// EnvVars are checked before GITHUB_TOKEN. Values may name env vars whose
	// values hold tokens, preserving the issue's configured-project-token order.
	EnvVars []string
	AllowGH bool
}

func (s EnvTokenSource) Token(ctx context.Context) (string, error) {
	for _, name := range s.EnvVars {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v, nil
		}
	}
	if v := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); v != "" {
		return v, nil
	}
	if s.AllowGH {
		cmd := exec.CommandContext(ctx, "gh", "auth", "token")
		out, err := cmd.Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			return strings.TrimSpace(string(out)), nil
		}
	}
	return "", ErrNoToken
}

func credentialHash(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:16]
}
