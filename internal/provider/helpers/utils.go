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

package helpers

import (
	"context"
	"fmt"
	"math/rand"
	"net/url"
	"strings"
	"time"

	fmc "github.com/netascode/go-fmc"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func GetStringList(result []gjson.Result) types.List {
	v := make([]attr.Value, len(result))
	for r := range result {
		v[r] = types.StringValue(result[r].String())
	}
	return types.ListValueMust(types.StringType, v)
}

func GetStringListFromStringSlice(result []string) types.List {
	v := make([]attr.Value, len(result))
	for i, e := range result {
		v[i] = types.StringValue(e)
	}
	return types.ListValueMust(types.StringType, v)
}

func GetInt64List(result []gjson.Result) types.List {
	v := make([]attr.Value, len(result))
	for r := range result {
		v[r] = types.Int64Value(result[r].Int())
	}
	return types.ListValueMust(types.Int64Type, v)
}

func GetStringSet(result []gjson.Result) types.Set {
	v := make([]attr.Value, len(result))
	for r := range result {
		v[r] = types.StringValue(result[r].String())
	}
	return types.SetValueMust(types.StringType, v)
}

func GetInt64Set(result []gjson.Result) types.Set {
	v := make([]attr.Value, len(result))
	for r := range result {
		v[r] = types.Int64Value(result[r].Int())
	}
	return types.SetValueMust(types.Int64Type, v)
}

// ToLower is the same as strings.ToLower, except it cares to not to convert null/unknown strings
// into empty strings.
func ToLower(s basetypes.StringValue) basetypes.StringValue {
	if s.IsUnknown() || s.IsNull() {
		return s
	}

	return types.StringValue(strings.ToLower(s.ValueString()))
}

// IsConfigUpdatingAt checks whether the attribute given by the Path is not Equal() between plan and state.
func IsConfigUpdatingAt(ctx context.Context, tfsdkPlan tfsdk.Plan, tfsdkState tfsdk.State, where path.Path) (bool, diag.Diagnostics) {
	var pv, sv attr.Value

	diags := tfsdkPlan.GetAttribute(ctx, where, &pv)
	if diags.HasError() {
		return false, diags
	}

	diags = tfsdkState.GetAttribute(ctx, where, &sv)
	if diags.HasError() {
		return false, nil
	}

	return !sv.Equal(pv), diags
}

// SetGjson conveniently wraps sjson.SetRaw, so that it acts on gjson.Result directly.
func SetGjson(orig gjson.Result, path string, content gjson.Result) gjson.Result {
	s, err := sjson.SetRaw(orig.String(), path, content.String())
	if err != nil {
		panic(err)
	}

	return gjson.Parse(s)
}

// IngestOnConflict handles the idempotency case where a POST returns HTTP 409 or
// HTTP 400 "already exists". It paginates the GET list endpoint until it finds an
// object matching the provided predicate, then fetches and returns that object's ID
// and full body.
//
// If postErr is not a conflict error, it is returned unchanged (caller should fail).
// If conflict resolution succeeds, (id, body, nil) is returned.
func IngestOnConflict(
	ctx context.Context,
	client *fmc.Client,
	resourcePath string,
	postErr error,
	postRes gjson.Result,
	attrLabel string,
	matches func(gjson.Result) bool,
	reqMods ...func(*fmc.Req),
) (string, gjson.Result, error) {
	if !(strings.Contains(postErr.Error(), "StatusCode 409") ||
		(strings.Contains(postErr.Error(), "StatusCode 400") && strings.Contains(postRes.String(), "already exists"))) {
		return "", postRes, postErr
	}
	tflog.Debug(ctx, fmt.Sprintf("IngestOnConflict: object already exists (409/400), searching by %s", attrLabel))
	offset := 0
	limit := 1000
	id := ""
	for {
		queryString := fmt.Sprintf("?limit=%d&offset=%d&expanded=true", limit, offset)
		listRes, listErr := client.Get(resourcePath+queryString, reqMods...)
		if listErr != nil {
			return "", listRes, fmt.Errorf("object already exists but failed to list (GET): %w", listErr)
		}
		for _, v := range listRes.Get("items").Array() {
			if matches(v) {
				id = v.Get("id").String()
				tflog.Debug(ctx, fmt.Sprintf("IngestOnConflict: found existing object (id=%s, attr=%s)", id, attrLabel))
				break
			}
		}
		if id != "" || !listRes.Get("paging.next.0").Exists() {
			break
		}
		offset += limit
	}
	if id == "" {
		return "", postRes, fmt.Errorf("object already exists (conflict) but could not be found by %s: %w", attrLabel, postErr)
	}
	body, err := client.Get(resourcePath+"/"+url.QueryEscape(id), reqMods...)
	if err != nil {
		return "", body, fmt.Errorf("object already exists (conflict) but failed to retrieve it (GET): %w", err)
	}
	return id, body, nil
}

func isRetryableError(err error, res gjson.Result) bool {
	msg := err.Error()
	// Connection-level errors and explicit rate-limit status.
	if strings.Contains(msg, "StatusCode 429") ||
		strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "connection refused") {
		return true
	}
	// FMC can return HTTP 400 for the parallel-lock advisory.
	// go-fmc populates res with the response body even on error, so we can
	// inspect the error description to distinguish it from "already exists".
	if strings.Contains(msg, "StatusCode 400") {
		desc := strings.ToLower(res.Get("error.messages.0.description").String())
		if strings.Contains(desc, "parallel") {
			return true
		}
	}
	return false
}

// RetryOnParallelLock retries fn when FMC signals that parallel write operations
// are blocked (HTTP 429 or HTTP 400 with parallel-lock advisory). A random 15–45 s
// delay is used between attempts so that concurrent provider instances retry at
// different times. Up to 10 attempts are made.
func RetryOnParallelLock(ctx context.Context, fn func() (gjson.Result, error)) (gjson.Result, error) {
	const maxAttempts = 10
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		res, err := fn()
		if err == nil || !isRetryableError(err, res) {
			return res, err
		}
		if attempt == maxAttempts {
			return res, err
		}
		delay := time.Duration(15+rand.Intn(31)) * time.Second
		tflog.Warn(ctx, fmt.Sprintf("FMC transient error (%s) — retrying in %s (attempt %d/%d)", err.Error(), delay, attempt, maxAttempts))
		time.Sleep(delay)
	}
	// unreachable
	return gjson.Result{}, nil
}

// DifferenceStringSet returns the elements that are present in `b`, but not in `a`.
func DifferenceStringSet(ctx context.Context, a basetypes.SetValue, b basetypes.SetValue) basetypes.SetValue {
	// extract elements from both sets
	var aElements []string
	var bElements []string
	a.ElementsAs(ctx, &aElements, false)
	b.ElementsAs(ctx, &bElements, false)

	diff := []attr.Value{}
	m := map[string]bool{}

	// Mark in `m` all elements from `a`
	for _, v := range aElements {
		m[v] = true
	}

	// Iterate over `b` to find elements not marked in `m`
	for _, v := range bElements {
		// If element is not in `m`, add it to the diff
		if _, ok := m[v]; !ok {
			diff = append(diff, types.StringValue(v))
		}
	}

	return types.SetValueMust(types.StringType, diff)
}
