// Copyright © 2023 Cisco Systems, Inc. and its affiliates.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestPlanMatchOnField(t *testing.T) {
	tests := []struct {
		name    string
		config  types.String
		planned types.String
		matchOn types.String
		want    types.String
	}{
		{"omitted, planned null (new item in existing list) — filled",
			types.StringNull(), types.StringNull(), types.StringValue("zone-a"), types.StringValue("zone-a")},
		{"omitted, planned unknown (new resource) — filled",
			types.StringNull(), types.StringUnknown(), types.StringValue("zone-a"), types.StringValue("zone-a")},
		{"omitted, planned null from broken state — repaired",
			types.StringNull(), types.StringNull(), types.StringValue("zone-a"), types.StringValue("zone-a")},
		{"omitted, match_on not known yet — unknown",
			types.StringNull(), types.StringNull(), types.StringUnknown(), types.StringUnknown()},
		{"omitted, match_on does not declare field — planned kept",
			types.StringNull(), types.StringNull(), types.StringNull(), types.StringNull()},
		{"set in config — config wins",
			types.StringValue("zone-b"), types.StringValue("zone-b"), types.StringValue("zone-a"), types.StringValue("zone-b")},
		{"set in config but unknown — planned kept",
			types.StringUnknown(), types.StringUnknown(), types.StringValue("zone-a"), types.StringUnknown()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planMatchOnField(tt.config, tt.planned, tt.matchOn)
			if !got.Equal(tt.want) {
				t.Fatalf("planMatchOnField() = %v, want %v", got, tt.want)
			}
		})
	}
}
