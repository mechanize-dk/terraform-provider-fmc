// Copyright © 2023 Cisco Systems, Inc. and its affiliates.
// SPDX-License-Identifier: MPL-2.0

// dup_recovery_autonat.go is the auto-NAT counterpart to dup_recovery.go
// (Patch 11, which handled manual NAT).
//
// FMC's duplicate-auto-NAT-rule response (HTTP 400) carries a generic
// description that names neither the existing rule's UUID nor the
// original-network object — e.g.
//
//	"The Auto NAT rule with the original source already exists
//	 Duplicate Auto NAT rule is not allowed"
//
// (verified against the lab FMC on 2026-06-04 — see
// tests/bulk_nat_tenants/PROBE_AUTONAT.md). So the recovery shape is
// "Branch B-prime":
//
//   1. detect the duplicate via the marker phrase
//   2. the *caller* supplies the originalNetwork.id it just POSTed —
//      FindDuplicateAutoNatRule searches the section for a rule with the
//      matching originalNetwork.id and returns it.
//
// This pushes the "what to look up" into the caller (where the request
// payload lives) and keeps the parser minimal.

package helpers

import (
	"context"
	"fmt"
	"strings"

	fmc "github.com/netascode/go-fmc"
	"github.com/tidwall/gjson"
)

// autoNatDupMarker is the substring we look for in FMC's duplicate-rule
// 400 response body. Pinned on the most specific phrase observed; if a
// future FMC release changes the wording, update this constant and
// dup_recovery_autonat_test.go in lockstep.
const autoNatDupMarker = "Duplicate Auto NAT rule"

// isAutoNatDuplicateMarker reports whether a string (typically an FMC error
// message or response body) contains the duplicate-auto-NAT-rule marker.
func isAutoNatDuplicateMarker(s string) bool {
	return strings.Contains(s, autoNatDupMarker)
}

// FindDuplicateAutoNatRule inspects an FMC POST error. If the body matches
// the duplicate marker, it GETs the section and returns the first rule
// whose originalNetwork.id equals the caller-supplied value.
//
// Returns:
//   - (foundRes, nil) when the marker matched and a rule with the matching
//     originalNetwork.id was found.
//   - (gjson.Result{}, postErr) when the marker did NOT match — caller
//     proceeds with the original POST error.
//   - (gjson.Result{}, wrappedErr) when the marker matched but the GET or
//     the lookup failed.
func FindDuplicateAutoNatRule(
	ctx context.Context,
	client *fmc.Client,
	resourcePath string,
	originalNetworkID string,
	postErr error,
	postRes gjson.Result,
	reqMods ...func(*fmc.Req),
) (gjson.Result, error) {
	if postErr == nil {
		return gjson.Result{}, nil
	}
	matched := false
	for _, s := range []string{postErr.Error(), postRes.String()} {
		if isAutoNatDuplicateMarker(s) {
			matched = true
			break
		}
	}
	if !matched {
		return gjson.Result{}, postErr
	}
	if originalNetworkID == "" {
		return gjson.Result{}, fmt.Errorf("duplicate auto-NAT rule but caller did not supply originalNetworkID; original POST error: %w", postErr)
	}
	listURL := resourcePath + "?expanded=true&limit=1000"
	listRes, err := client.Get(listURL, reqMods...)
	if err != nil {
		return gjson.Result{}, fmt.Errorf("duplicate auto-NAT rule but GET failed: %w (original POST error: %v)", err, postErr)
	}
	for _, it := range listRes.Get("items").Array() {
		if it.Get("originalNetwork.id").String() == originalNetworkID {
			return it, nil
		}
	}
	return gjson.Result{}, fmt.Errorf("duplicate auto-NAT rule reported but no rule with originalNetwork.id=%s found in section; original POST error: %w", originalNetworkID, postErr)
}
