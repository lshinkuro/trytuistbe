package config

import (
	"strings"
	"testing"
)

func TestLoadProdRejectsExampleSecrets(t *testing.T) {
	t.Setenv("APP_ENV", "prod")
	t.Setenv("JWT_SECRET", strings.Repeat("x", 64))

	t.Setenv("API_KEYS", "")
	if _, err := Load(); err == nil {
		t.Fatal("prod tanpa API_KEYS harus ditolak (tidak boleh jatuh ke key dev)")
	}
	t.Setenv("API_KEYS", "prod-api-key-change-me")
	if _, err := Load(); err == nil {
		t.Fatal("prod dengan key contoh harus ditolak")
	}
	t.Setenv("API_KEYS", "k3y-acak-yang-panjang")
	cfg, err := Load()
	if err != nil || cfg.LogLevel.String() != "INFO" {
		t.Fatalf("prod valid: %v, log level %v", err, cfg.LogLevel)
	}
}
