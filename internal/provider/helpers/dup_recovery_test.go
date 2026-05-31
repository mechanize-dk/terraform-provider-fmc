// Copyright © 2023 Cisco Systems, Inc. and its affiliates.
// SPDX-License-Identifier: MPL-2.0

package helpers

import (
	"errors"
	"testing"

	"github.com/tidwall/gjson"
)

func TestExtractDuplicateIndex(t *testing.T) {
	tests := []struct {
		name    string
		errStr  string
		resStr  string
		wantIdx int
		wantOk  bool
	}{
		{
			name:    "FMC duplicate error in res body",
			errStr:  "StatusCode 400",
			resStr:  `{"error":{"messages":[{"description":"Validation failed due to duplicate Manual NAT rules in Policy Please remove the duplicate rules applicable to All Devices : Duplicate entry of ManualNatRule at rule index (3) is also present at rule index [5]"}]}}`,
			wantIdx: 5,
			wantOk:  true,
		},
		{
			name:    "FMC duplicate error in err string",
			errStr:  `HTTP Request failed: StatusCode 400, {"error":{"messages":[{"description":"Duplicate entry of ManualNatRule at rule index (0) is also present at rule index [42]"}]}}`,
			resStr:  "",
			wantIdx: 42,
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
			resStr:  `{"error":{"messages":[{"description":"Some text mentioning rule index [5] without the duplicate prefix"}]}}`,
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
