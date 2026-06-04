// Copyright © 2023 Cisco Systems, Inc. and its affiliates.
// SPDX-License-Identifier: MPL-2.0

// nat_match.go is part of the mechanize-dk fork's bulk NAT resources
// (fmc_mze_manual_nat_rules and fmc_mze_auto_nat_rules). MatchOn captures
// a tenant-scope predicate — currently the source and/or destination
// interface ID — and exposes Validate (plan-time check), AutoFill
// (Create-time injection), and Hash (synthetic resource ID).
//
// match_on is NOT a runtime ownership boundary; it's an authoring tool. See
// "Cooperative ownership" in docs/superpowers/specs/2026-06-04-bulk-nat-tenant-resources-design.md.

package helpers

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// MatchOn captures the tenant scope. v1 supports two fields; additions are
// non-breaking.
type MatchOn struct {
	SourceInterfaceID      string
	DestinationInterfaceID string
}

// IsEmpty reports whether no field is set. A resource configured with an
// empty MatchOn is invalid (the schema requires at least one field).
func (m MatchOn) IsEmpty() bool {
	return m.SourceInterfaceID == "" && m.DestinationInterfaceID == ""
}

// Hash returns a 16-char hex prefix of the sha256 of the canonical form of
// the MatchOn. Used as part of the synthetic resource ID.
//
// The canonical form is "<field-name>=<value>;..." with field names sorted
// alphabetically, so semantically equal MatchOns hash identically regardless
// of struct field order.
func (m MatchOn) Hash() string {
	pairs := []string{}
	if m.SourceInterfaceID != "" {
		pairs = append(pairs, fmt.Sprintf("source_interface_id=%s", m.SourceInterfaceID))
	}
	if m.DestinationInterfaceID != "" {
		pairs = append(pairs, fmt.Sprintf("destination_interface_id=%s", m.DestinationInterfaceID))
	}
	sort.Strings(pairs)
	canonical := ""
	for i, p := range pairs {
		if i > 0 {
			canonical += ";"
		}
		canonical += p
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])[:16]
}
