// Package migrations embeds the SQL files alongside it.
//
// This directory is a Go package purely so the migration runner can carry the
// SQL inside its binary. Embedding rather than reading from disk means the
// image that runs a migration contains the exact statements it will execute —
// so "which version is this database on, and what did that migration actually
// say" stays answerable from an image tag months later, during the kind of
// investigation where the working tree has long since moved on.
//
// Go's embed cannot reach outside its own package directory, which is the whole
// reason this file exists rather than a //go:embed in cmd/migrate.
package migrations

import "embed"

// FS holds every .sql file in this directory, up and down alike.
//
//go:embed *.sql
var FS embed.FS
