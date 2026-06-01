// Copyright © 2023 Cisco Systems, Inc. and its affiliates.
// SPDX-License-Identifier: MPL-2.0

package helpers

import (
	"context"
	"errors"
	"net/http"
	"testing"

	fmc "github.com/netascode/go-fmc"
	"github.com/tidwall/gjson"
	gock "gopkg.in/h2non/gock.v1"
)

// TestExtractDuplicateIndex covers the regex extraction. FMC's full error is
//
//	"Duplicate entry of ManualNatRule at rule index (N) is also present at rule index [M]"
//
// where (N) is the existing duplicate's 1-indexed position and [M] is the
// would-be position of the new rule. We must capture (N) — verified against a
// live FMC on 2026-06-01.
func TestExtractDuplicateIndex(t *testing.T) {
	tests := []struct {
		name    string
		errStr  string
		resStr  string
		wantIdx int
		wantOk  bool
	}{
		{
			name:    "FMC duplicate error in res body — captures parens (N), not brackets [M]",
			errStr:  "StatusCode 400",
			resStr:  `{"error":{"messages":[{"description":"Validation failed due to duplicate Manual NAT rules in Policy Please remove the duplicate rules applicable to All Devices : Duplicate entry of ManualNatRule at rule index (3) is also present at rule index [5]"}]}}`,
			wantIdx: 3,
			wantOk:  true,
		},
		{
			name:    "FMC duplicate error in err string — captures parens, not brackets",
			errStr:  `HTTP Request failed: StatusCode 400, {"error":{"messages":[{"description":"Duplicate entry of ManualNatRule at rule index (7) is also present at rule index [42]"}]}}`,
			resStr:  "",
			wantIdx: 7,
			wantOk:  true,
		},
		{
			name:    "No match — generic error",
			errStr:  "StatusCode 500 Internal Server Error",
			resStr:  "",
			wantIdx: 0,
			wantOk:  false,
		},
		{
			name:    "No match — different 400 body",
			errStr:  "StatusCode 400",
			resStr:  `{"error":{"messages":[{"description":"Translated Service cannot be empty when Original Service is Selected"}]}}`,
			wantIdx: 0,
			wantOk:  false,
		},
		{
			name:    "Nil error returns no match",
			errStr:  "",
			resStr:  "",
			wantIdx: 0,
			wantOk:  false,
		},
		{
			name:    "Word 'index' alone does not false-positive",
			errStr:  "StatusCode 400",
			resStr:  `{"error":{"messages":[{"description":"Some text mentioning rule index (5) without the duplicate prefix"}]}}`,
			wantIdx: 0,
			wantOk:  false,
		},
		{
			name:    "Brackets-only [M] without parens — no match (we anchor on parens)",
			errStr:  "StatusCode 400",
			resStr:  `{"error":{"messages":[{"description":"Duplicate entry of ManualNatRule at rule index [5]"}]}}`,
			wantIdx: 0,
			wantOk:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.errStr != "" {
				err = errors.New(tc.errStr)
			}
			res := gjson.Parse(tc.resStr)
			idx, ok := extractDuplicateIndex(err, res)
			if ok != tc.wantOk {
				t.Errorf("ok = %v, want %v", ok, tc.wantOk)
			}
			if idx != tc.wantIdx {
				t.Errorf("idx = %d, want %d", idx, tc.wantIdx)
			}
		})
	}
}

// TestFindDuplicateByIndex_OffsetMapping locks in the conversion from FMC's
// 1-indexed `(N)` in the error to `?offset=N-1` on the list endpoint. Without
// this round-trip test the off-by-one bug we found on 2026-06-01 would
// resurface if a future refactor "tidied" the arithmetic.
func TestFindDuplicateByIndex_OffsetMapping(t *testing.T) {
	defer gock.Off()

	const baseURL = "https://test.example"
	const resourcePath = "/api/fmc_config/v1/domain/X/policy/ftdnatpolicies/Y/manualnatrules"
	const existingID = "00000000-0000-0ed3-0000-000000000003"

	// Auth + version mocks for NewClient.
	gock.New(baseURL).Post("/api/fmc_platform/v1/auth/generatetoken").
		Reply(204).SetHeader("X-auth-access-token", "fake-token")
	gock.New(baseURL).Get("/api/fmc_platform/v1/info/serverversion").
		Reply(200).BodyString(`{"items":[{"serverVersion":"7.2.4 (build 123)"}]}`)

	// Dup-recovery GET: FMC error says (3) → helper must request offset=2.
	gock.New(baseURL).
		Get(resourcePath).
		MatchParam("offset", "2").
		MatchParam("limit", "1").
		MatchParam("expanded", "true").
		Reply(200).
		BodyString(`{"items":[{"id":"` + existingID + `","natType":"STATIC"}],"paging":{"count":1}}`)

	httpClient := &http.Client{}
	gock.InterceptClient(httpClient)

	client, err := fmc.NewClient(baseURL, "u", "p", fmc.CustomHttpClient(httpClient), fmc.MaxRetries(0))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	postErr := errors.New(`HTTP Request failed: StatusCode 400, {"error":{"messages":[{"description":"Duplicate entry of ManualNatRule at rule index (3) is also present at rule index [6]"}]}}`)

	res, err := FindDuplicateByIndex(context.Background(), &client, resourcePath, postErr, gjson.Result{})
	if err != nil {
		t.Fatalf("FindDuplicateByIndex returned error: %v", err)
	}
	if got := res.Get("id").String(); got != existingID {
		t.Errorf("returned item id = %q, want %q", got, existingID)
	}
	if !gock.IsDone() {
		t.Errorf("not all gock mocks were consumed: %v", gock.Pending())
	}
}

// TestFindDuplicateByIndex_NoMatch_ReturnsOriginalError ensures that when the
// regex does NOT match (any non-duplicate error), the helper returns the
// caller's original error untouched and does NOT make a GET request.
func TestFindDuplicateByIndex_NoMatch_ReturnsOriginalError(t *testing.T) {
	defer gock.Off()

	const baseURL = "https://test.example"

	gock.New(baseURL).Post("/api/fmc_platform/v1/auth/generatetoken").
		Reply(204).SetHeader("X-auth-access-token", "fake-token")
	gock.New(baseURL).Get("/api/fmc_platform/v1/info/serverversion").
		Reply(200).BodyString(`{"items":[{"serverVersion":"7.2.4 (build 123)"}]}`)

	httpClient := &http.Client{}
	gock.InterceptClient(httpClient)

	client, err := fmc.NewClient(baseURL, "u", "p", fmc.CustomHttpClient(httpClient), fmc.MaxRetries(0))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	original := errors.New("StatusCode 500 Internal Server Error")
	_, err = FindDuplicateByIndex(context.Background(), &client, "/whatever", original, gjson.Result{})
	if !errors.Is(err, original) && err != original {
		t.Errorf("expected original error to be returned; got %v", err)
	}
}
