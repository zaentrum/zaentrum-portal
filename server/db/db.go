// Package db embeds the portal registry SQL migrations so the service can apply
// them at boot (no separate init job). Files are applied in lexical order and
// are all idempotent, so re-running on every start is safe.
package db

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS
