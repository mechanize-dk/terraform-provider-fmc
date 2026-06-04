// Copyright © 2023 Cisco Systems, Inc. and its affiliates.
// SPDX-License-Identifier: MPL-2.0

// This file is part of the mechanize-dk fork of terraform-provider-fmc and
// has no upstream counterpart. It tracks the fork's internal patch-level
// version separately from the upstream provider release (which is tracked
// via git tags + goreleaser).

package helpers

import "log"

// _VERSION is the mechanize-dk fork's internal patch-level marker. Bump it
// on every code change to fork-owned files using semantic versioning:
//
//	PATCH (0.0.x): bug fix
//	MINOR (0.x.0): new feature or behavior change in a fork patch
//	MAJOR (x.0.0): breaking change to a fork patch's contract
//
// The upstream provider version (e.g. v2.4.1) tracks the release published
// to registry.terraform.io/mechanize-dk/fmc and is set at tag time by
// goreleaser — not here.
const _VERSION = "0.2.7"

// init emits the fork's patch-level version once at package load. Visible to
// the user via `TF_LOG=INFO terraform apply` (or any other log level that
// surfaces [INFO] lines). Hooked here rather than in FmcProvider.Configure
// because Configure lives inside the //template:begin provider block in
// internal/provider/provider.go and would be regenerated away on every
// `go generate` — this init lives entirely in a fork-only file.
func init() {
	log.Printf("[INFO] mechanize-dk/terraform-provider-fmc fork _VERSION=%s", _VERSION)
}
