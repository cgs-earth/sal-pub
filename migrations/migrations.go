// Package migrations embeds the numbered SQL migrations so the server can
// apply them at startup.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
