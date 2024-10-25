// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import (
	"forgejo.org/modules/log"
)

// Annex represents the configuration for git-annex
var Annex = struct {
	Enabled        bool `ini:"ENABLED"`
	DisableP2PHTTP bool `ini:"DISABLE_P2PHTTP"`
}{}

func loadAnnexFrom(rootCfg ConfigProvider) {
	sec := rootCfg.Section("annex")
	if err := sec.MapTo(&Annex); err != nil {
		log.Fatal("Failed to map Annex settings: %v", err)
	}
	if !sec.HasKey("DISABLE_P2PHTTP") {
		// If DisableP2PHTTP is not explicitly set then use DisableHTTPGit as its default
		Annex.DisableP2PHTTP = Repository.DisableHTTPGit
	}
}
