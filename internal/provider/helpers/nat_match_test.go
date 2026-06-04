// Copyright © 2023 Cisco Systems, Inc. and its affiliates.
// SPDX-License-Identifier: MPL-2.0

package helpers

import "testing"

func TestMatchOn_Hash_Deterministic(t *testing.T) {
	a := MatchOn{SourceInterfaceID: "zone-a"}
	b := MatchOn{SourceInterfaceID: "zone-a"}
	if a.Hash() != b.Hash() {
		t.Fatalf("Hash() not deterministic: a=%q b=%q", a.Hash(), b.Hash())
	}
	if len(a.Hash()) != 16 {
		t.Fatalf("Hash() length = %d, want 16", len(a.Hash()))
	}
}

func TestMatchOn_Hash_DiffersByField(t *testing.T) {
	a := MatchOn{SourceInterfaceID: "zone-a"}
	b := MatchOn{SourceInterfaceID: "zone-b"}
	if a.Hash() == b.Hash() {
		t.Fatal("Hash() should differ for different source_interface_id")
	}
	c := MatchOn{SourceInterfaceID: "zone-a", DestinationInterfaceID: "zone-b"}
	if a.Hash() == c.Hash() {
		t.Fatal("Hash() should differ when adding destination_interface_id")
	}
}
