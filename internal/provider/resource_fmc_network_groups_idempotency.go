// Copyright © 2023 Cisco Systems, Inc. and its affiliates.
// All rights reserved.
//
// Licensed under the Mozilla Public License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://mozilla.org/MPL/2.0/
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: MPL-2.0

package provider

// Fork patch — not generated. See PATCHES.md § Patch 2.
//
// networkGroupsFindOrCreate is the "find first" idempotency fallback for
// fmc_network_groups bulk creates. When the bulk POST returns a conflict it is
// called instead of the paginate-all approach. For each group it uses the
// nameOrValue filter to look it up; if found it imports the ID, if not it POSTs
// to create it.

import (
	"context"
	"fmt"
	"net/url"

	fmc "github.com/netascode/go-fmc"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func networkGroupsFindOrCreate(
	ctx context.Context,
	plan NetworkGroups,
	ret *NetworkGroups,
	bulk *networkGroupsBulk,
	bodyParts []string,
	client *fmc.Client,
	reqMods ...func(*fmc.Req),
) diag.Diagnostics {
	if ret.Items == nil && len(bulk.groups) != 0 {
		ret.Items = map[string]NetworkGroupsItems{}
	}
	for i, g := range bulk.groups {
		name := g.name

		// Check if the group already exists using a targeted filter query.
		filterQuery := "?limit=1000&expanded=true&filter=" + url.QueryEscape("nameOrValue:"+name)
		listRes, listErr := client.Get(plan.getPath()+filterQuery, reqMods...)
		if listErr != nil {
			return diag.Diagnostics{
				diag.NewErrorDiagnostic("Client Error", fmt.Sprintf(
					"Network group '%s': failed to query existing objects: %s, %s", name, listErr, listRes.String())),
			}
		}
		var existingID, existingType string
		for _, v := range listRes.Get("items").Array() {
			if v.Get("name").String() == name {
				existingID = v.Get("id").String()
				existingType = v.Get("type").String()
				break
			}
		}

		if existingID != "" {
			// Group already exists — import its ID.
			tmp := plan.Items[name]
			tmp.Id = types.StringValue(existingID)
			tmp.Type = types.StringValue(existingType)
			ret.Items[name] = tmp
			tflog.Debug(ctx, fmt.Sprintf("%s: Network group '%s' already exists (id=%s), imported",
				plan.Id.ValueString(), name, existingID))
		} else {
			// Group does not exist — create it.
			itemRes, itemErr := client.Post(plan.getPath(), bodyParts[i], reqMods...)
			if itemErr != nil {
				return diag.Diagnostics{
					diag.NewErrorDiagnostic("Client Error", fmt.Sprintf(
						"Failed to create network group '%s': %s, %s", name, itemErr, itemRes.String())),
				}
			}
			tmp := plan.Items[name]
			tmp.Id = types.StringValue(itemRes.Get("id").String())
			tmp.Type = types.StringValue(itemRes.Get("type").String())
			ret.Items[name] = tmp
		}
	}
	return nil
}
