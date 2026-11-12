// Package templates embeds the starter .devcontainer files written by
// `dev new`. Each released starter version lives in devcontainer/vN/ and must
// never change once released (its hash is registered in internal/devcontainer).
package templates

import "embed"

// FS holds devcontainer/v1/devcontainer.json, devcontainer/v1/Dockerfile, ...
//
//go:embed all:devcontainer
var FS embed.FS
