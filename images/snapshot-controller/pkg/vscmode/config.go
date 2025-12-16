/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vscmode

import (
	"os"
	"strings"
)

const (
	// EnvVSCMode is the environment variable name for VSC mode configuration
	EnvVSCMode = "SNAPSHOT_CONTROLLER_VSC_MODE"

	// ModeLegacy is the legacy mode (upstream behavior)
	ModeLegacy = "legacy"

	// ModeVSCOnly is the VSC-only mode (downstream extension)
	ModeVSCOnly = "vsc-only"
)

// GetVSCMode returns the configured VSCMode instance.
// Priority:
// 1. Environment variable SNAPSHOT_CONTROLLER_VSC_MODE
// 2. Default: VSCOnlyMode (for Deckhouse builds)
func GetVSCMode() VSCMode {
	mode := os.Getenv(EnvVSCMode)
	mode = strings.ToLower(strings.TrimSpace(mode))

	switch mode {
	case ModeLegacy:
		return NewLegacyVSCMode()
	case ModeVSCOnly, "":
		// Default to VSC-only mode for Deckhouse builds
		return NewVSCOnlyMode()
	default:
		// Unknown mode, default to VSC-only
		return NewVSCOnlyMode()
	}
}
