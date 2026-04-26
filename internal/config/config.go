// Package config loads runtime configuration from environment variables.
//
// Why a dedicated package?
//   1. One place to look when you ask "what config does this app read?"
//   2. Validation happens once, at startup - fail fast if anything's missing.
//   3. The rest of the code reads typed fields (cfg.Port int) instead of
//      sprinkling os.Getenv("PORT") + strconv.Atoi everywhere.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the typed view of every env var the app understands.
// Each field maps to one variable in .env.example.
type Config struct {
	Port           int
	DatabaseURL    string
	RedisURL       string
	JWTSecret      []byte // []byte because the JWT library wants bytes for HMAC
	LogLevel       string
	SeedOnStart    bool
	SeedFilePath   string
	MigrationsDir  string
	StreamName     string
	ConsumerGroup  string
	ConsumerName   string
}

// Load reads env vars, validates them, and returns a populated Config.
// Returns an aggregated error listing every missing/invalid variable so the
// operator sees them all at once, not one fix-restart cycle per missing var.
func Load() (*Config, error) {
	var errs []string

	port, err := atoiDefault("PORT", 8080)
	if err != nil {
		errs = append(errs, err.Error())
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		errs = append(errs, "DATABASE_URL is required")
	}

	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		errs = append(errs, "REDIS_URL is required")
	}

	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		errs = append(errs, "JWT_SECRET is required")
	}

	cfg := &Config{
		Port:          port,
		DatabaseURL:   dbURL,
		RedisURL:      redisURL,
		JWTSecret:     []byte(jwtSecret),
		LogLevel:      getenv("LOG_LEVEL", "info"),
		SeedOnStart:   getenv("SEED_ON_START", "true") == "true",
		SeedFilePath:  getenv("SEED_FILE_PATH", "./nevup_seed_dataset.csv"),
		MigrationsDir: getenv("MIGRATIONS_DIR", "./migrations"),
		StreamName:    getenv("STREAM_NAME", "trade-events"),
		ConsumerGroup: getenv("CONSUMER_GROUP", "metrics-workers"),
		ConsumerName:  getenv("CONSUMER_NAME", "worker-1"),
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return cfg, nil
}

// getenv returns the env var, or the fallback if it's unset/empty.
// Lowercase first letter = unexported = private to this package.
func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// atoiDefault parses an integer env var, returning the fallback if unset.
// Errors only on a malformed value (e.g., PORT="abc").
func atoiDefault(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, errors.New(key + " must be an integer, got " + v)
	}
	return n, nil
}
