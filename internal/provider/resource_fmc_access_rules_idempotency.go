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

// Fork patch — not generated. See PATCHES.md § Patch 3.
//
// accessRulesFindOrCreate is the "find first" idempotency fallback for
// fmc_access_rules bulk creates. When the bulk POST returns a conflict it is
// called instead of the paginate-all approach. For each rule it uses the
// name filter to look it up; if found it imports the ID, if not it POSTs to
// create it.

import (
	"context"
	"fmt"
	"net/url"

	fmc "github.com/netascode/go-fmc"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/tidwall/gjson"
)

func accessRulesFindOrCreate(
	ctx context.Context,
	client *fmc.Client,
	plan AccessRules,
	bulk *AccessRules,
	itemBodies []gjson.Result,
	individualURLParams string,
	reqMods ...func(*fmc.Req),
) error {
	for i := range bulk.Items {
		name := bulk.Items[i].Name.ValueString()

		// Check if the rule already exists using a targeted filter query.
		filterQuery := "?limit=1000&expanded=true&filter=" + url.QueryEscape("name:"+name)
		listRes, listErr := client.Get(plan.getPath()+filterQuery, reqMods...)
		if listErr != nil {
			return fmt.Errorf("access rule '%s': failed to query existing rules: %s, %s", name, listErr, listRes.String())
		}
		var existingID string
		for _, v := range listRes.Get("items").Array() {
			if v.Get("name").String() == name {
				existingID = v.Get("id").String()
				break
			}
		}

		if existingID != "" {
			// Rule already exists — import its ID.
			bulk.Items[i].Id = types.StringValue(existingID)
			tflog.Debug(ctx, fmt.Sprintf("Access rule '%s' already exists (id=%s), imported", name, existingID))
		} else {
			// Rule does not exist — create it.
			var itemBody string
			if i < len(itemBodies) {
				itemBody = itemBodies[i].Raw
			}
			itemRes, itemErr := client.Post(plan.getPath()+individualURLParams, itemBody, reqMods...)
			if itemErr != nil {
				return itemErr
			}
			bulk.Items[i].Id = types.StringValue(itemRes.Get("id").String())
		}
	}
	return nil
}
