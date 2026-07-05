// Package config reads the portal-api runtime configuration from the
// environment. Blank optional values disable the corresponding feature.
package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	// HTTP
	Port string // SERVER_PORT / PORT (default 8080)

	// Database. DatabaseURL is a libpq/pgx DSN or URL; user/password override
	// when non-empty (parity with the katalog-manager store).
	DatabaseURL      string // PG_URL / DATABASE_URL
	DatabaseUser     string // DATABASE_USER
	DatabasePassword string // DATABASE_PASSWORD

	// Auth (bearer JWT, issuer-only MVP — audience optional).
	OIDCIssuer       string // OIDC_ISSUER
	Audience         string // OIDC_AUDIENCE (default "chino")
	AudienceRequired bool   // OIDC_AUDIENCE_REQUIRED (default false → issuer-only)
	AuthDisabled     bool   // AUTH_DISABLED (default false)

	// Realm role that authorizes registry writes (settings admin).
	AdminRole string // PORTAL_ADMIN_ROLE (default "zaentrum-admin")

	// Operator / instances console.
	InstanceSelector string   // PORTAL_INSTANCE_SELECTOR — label filter for listed deployments (default "" = all in ns)
	ProtectedNames   []string // PORTAL_PROTECT — deployments the UI must not scale/restart (default postgres,kafka,valkey,keycloak)
	OperatorGroup    string   // PORTAL_OPERATOR_GROUP (default zaentrum.io)
	OperatorVersion  string   // PORTAL_OPERATOR_VERSION (default v1alpha1)
	OperatorPlural   string   // PORTAL_OPERATOR_PLURAL (default zaentrums)
}

func env(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func envDefault(def string, keys ...string) string {
	if v := env(keys...); v != "" {
		return v
	}
	return def
}

func envBool(def bool, keys ...string) bool {
	v := env(keys...)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// normalizeDSN makes a Spring/JDBC-style datasource URL pgx-friendly: it strips
// a leading `jdbc:` and, when no sslmode is given, defaults to `disable` (the
// in-cluster demo Postgres is plaintext; a TLS deployment sets sslmode itself).
func normalizeDSN(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "jdbc:")
	if s == "" {
		return s
	}
	if (strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://")) && !strings.Contains(s, "sslmode=") {
		sep := "?"
		if strings.Contains(s, "?") {
			sep = "&"
		}
		s += sep + "sslmode=disable"
	}
	return s
}

// Load reads configuration from the process environment.
func Load() Config {
	return Config{
		Port:             envDefault("8080", "SERVER_PORT", "PORT"),
		DatabaseURL:      normalizeDSN(env("PG_URL", "DATABASE_URL", "SPRING_DATASOURCE_URL")),
		DatabaseUser:     env("DATABASE_USER", "SPRING_DATASOURCE_USERNAME"),
		DatabasePassword: env("DATABASE_PASSWORD", "SPRING_DATASOURCE_PASSWORD"),

		OIDCIssuer:       env("OIDC_ISSUER", "SPRING_SECURITY_OAUTH2_RESOURCESERVER_JWT_ISSUER_URI"),
		Audience:         envDefault("chino", "OIDC_AUDIENCE"),
		AudienceRequired: envBool(false, "OIDC_AUDIENCE_REQUIRED"),
		AuthDisabled:     envBool(false, "AUTH_DISABLED"),

		AdminRole: envDefault("zaentrum-admin", "PORTAL_ADMIN_ROLE"),

		InstanceSelector: env("PORTAL_INSTANCE_SELECTOR"),
		ProtectedNames:   splitCSV(envDefault("postgres,kafka,valkey,keycloak", "PORTAL_PROTECT")),
		OperatorGroup:    envDefault("zaentrum.io", "PORTAL_OPERATOR_GROUP"),
		OperatorVersion:  envDefault("v1alpha1", "PORTAL_OPERATOR_VERSION"),
		OperatorPlural:   envDefault("zaentrums", "PORTAL_OPERATOR_PLURAL"),
	}
}

// splitCSV parses a comma-separated list, trimming blanks.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
