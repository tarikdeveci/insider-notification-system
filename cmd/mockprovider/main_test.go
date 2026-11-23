package main

import (
	"strings"
	"testing"
)

func TestLoadConfigNamesVariableNotValue(t *testing.T) {
	cases := map[string]string{
		"MOCK_FAILURE_RATE":   "NaN",
		"MOCK_FAILURE_STATUS": "100",
		"MOCK_LATENCY_MS":     "99999",
	}
	for name, val := range cases {
		_, _, err := loadConfig(func(k string) string {
			if k == name {
				return val
			}
			return ""
		})
		if err == nil || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), val) {
			t.Fatalf("%s=%s: err = %v", name, val, err)
		}
	}
	if _, _, err := loadConfig(func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
}
