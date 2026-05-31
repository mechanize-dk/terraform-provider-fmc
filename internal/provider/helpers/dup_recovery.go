// Copyright © 2023 Cisco Systems, Inc. and its affiliates.
// SPDX-License-Identifier: MPL-2.0

package helpers

import (
	"context"
	"fmt"
	"regexp"
	"strconv"

	fmc "github.com/netascode/go-fmc"
	"github.com/tidwall/gjson"
)

// dupIndexRegex matches FMC's content-level "duplicate at index" error. The
// pattern is anchored on both "Duplicate entry" and "rule index [N]" so it
// does not false-positive on unrelated messages that happen to mention an
// index.
var dupIndexRegex = regexp.MustCompile(`Duplicate entry .* at rule index \[(\d+)\]`)

// extractDuplicateIndex scans an FMC error and response body for the
// "Duplicate entry … at rule index [N]" pattern and returns N together with
// a bool indicating whether the pattern matched.
//
// Both the err string and the res string are scanned because FMC sometimes
// nests the descriptive text in either place.
func extractDuplicateIndex(err error, res gjson.Result) (int, bool) {
	if err == nil {
		return 0, false
	}
	candidates := []string{err.Error(), res.String()}
	for _, s := range candidates {
		if m := dupIndexRegex.FindStringSubmatch(s); m != nil {
			idx, perr := strconv.Atoi(m[1])
			if perr != nil {
				continue
			}
			return idx, true
		}
	}
	return 0, false
}

// FindDuplicateByIndex inspects an FMC POST error. If the error body matches
// FMC's "duplicate at index" pattern (currently emitted only for
// fmc_ftd_manual_nat_rule), it extracts the existing object's index, GETs the
// collection at that index, and returns the existing object's JSON.
//
// Returns:
//   - (foundRes, nil) when the pattern matched and the GET succeeded.
//     foundRes is items[0] of the response (a single rule).
//   - (gjson.Result{}, postErr) when the pattern did NOT match. Caller
//     proceeds with the original fail behavior.
//   - (gjson.Result{}, wrappedErr) when the pattern matched but the GET or
//     parsing failed.
func FindDuplicateByIndex(
	ctx context.Context,
	client *fmc.Client,
	resourcePath string,
	postErr error,
	postRes gjson.Result,
	reqMods ...func(*fmc.Req),
) (gjson.Result, error) {
	idx, ok := extractDuplicateIndex(postErr, postRes)
	if !ok {
		return gjson.Result{}, postErr
	}
	listURL := resourcePath + fmt.Sprintf("?expanded=true&offset=%d&limit=1", idx)
	listRes, err := client.Get(listURL, reqMods...)
	if err != nil {
		return gjson.Result{}, fmt.Errorf("duplicate at index %d but GET failed: %w (original POST error: %v)", idx, err, postErr)
	}
	item := listRes.Get("items.0")
	if !item.Exists() {
		return gjson.Result{}, fmt.Errorf("duplicate at index %d but no item returned from GET; original POST error: %w", idx, postErr)
	}
	return item, nil
}
