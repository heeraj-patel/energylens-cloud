package migrations

import "embed"

// FS is the versioned SQL applied only by the dedicated migrate command.
//
//go:embed 0*.sql
var FS embed.FS
