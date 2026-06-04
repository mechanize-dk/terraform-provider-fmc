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

func TestMatchOn_Validate(t *testing.T) {
	m := MatchOn{SourceInterfaceID: "zone-a"}
	tests := []struct {
		name    string
		rule    map[string]*string
		wantErr bool
	}{
		{"absent — passes (will be auto-filled)", map[string]*string{}, false},
		{"matches — passes", map[string]*string{"source_interface_id": ptr("zone-a")}, false},
		{"conflicts — fails", map[string]*string{"source_interface_id": ptr("zone-b")}, true},
		{"explicitly null — fails (rules cannot opt out)", map[string]*string{"source_interface_id": nil}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := m.Validate("test_item", tt.rule)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func ptr(s string) *string { return &s }

func TestMatchOn_AutoFill(t *testing.T) {
	m := MatchOn{SourceInterfaceID: "zone-a", DestinationInterfaceID: "zone-z"}

	// Empty item gets both fields filled.
	item := map[string]*string{}
	m.AutoFill(item)
	if got := item["source_interface_id"]; got == nil || *got != "zone-a" {
		t.Errorf("source_interface_id not auto-filled, got=%v", got)
	}
	if got := item["destination_interface_id"]; got == nil || *got != "zone-z" {
		t.Errorf("destination_interface_id not auto-filled, got=%v", got)
	}

	// Already-set item is not overwritten.
	pre := map[string]*string{"source_interface_id": ptr("zone-a")}
	m.AutoFill(pre)
	if *pre["source_interface_id"] != "zone-a" {
		t.Errorf("AutoFill clobbered a matching set field")
	}

	// Empty matcher fields aren't injected.
	m2 := MatchOn{SourceInterfaceID: "zone-a"} // destination unset
	item2 := map[string]*string{}
	m2.AutoFill(item2)
	if _, present := item2["destination_interface_id"]; present {
		t.Errorf("AutoFill injected empty matcher field")
	}
}
