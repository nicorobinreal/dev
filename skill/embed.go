// Package skillfiles embeds the dev agent skill (dev/SKILL.md and any
// references) that internal/skill writes into each harness's skills folder.
package skillfiles

import "embed"

// FS holds dev/SKILL.md and the rest of the skill folder.
//
//go:embed all:dev
var FS embed.FS

// Root is the folder inside FS that becomes <skills>/dev/.
const Root = "dev"
