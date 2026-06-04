// Copyright © 2023 Cisco Systems, Inc. and its affiliates.
// SPDX-License-Identifier: MPL-2.0

package helpers

import "testing"

func TestIsAutoNatDuplicateMarker(t *testing.T) {
	// The exact error description observed against the lab FMC 2026-06-04
	// (see tests/bulk_nat_tenants/PROBE_AUTONAT.md).
	dupBody := `{"error":{"category":"FRAMEWORK","messages":[{"description":"The Auto NAT rule with the original source already exists Duplicate Auto NAT rule is not allowed"}],"severity":"ERROR"}}`
	if !isAutoNatDuplicateMarker(dupBody) {
		t.Fatal("expected duplicate-marker match on observed FMC error body")
	}
}

func TestIsAutoNatDuplicateMarker_NoMatch(t *testing.T) {
	other := `{"error":{"messages":[{"description":"some unrelated error"}]}}`
	if isAutoNatDuplicateMarker(other) {
		t.Fatal("expected no match for unrelated error")
	}
}
