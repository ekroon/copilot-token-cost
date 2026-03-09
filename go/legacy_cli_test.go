package main

import "testing"

func TestParseExplicitPeriodValue(t *testing.T) {
	testCases := []struct {
		name  string
		input string
		want  dateWindowSpec
	}{
		{name: "today", input: "today", want: dateWindowSpec{Mode: "today"}},
		{name: "yesterday", input: "yesterday", want: dateWindowSpec{Mode: "yesterday"}},
		{name: "all", input: "all", want: dateWindowSpec{AllTime: true}},
		{name: "numeric days", input: "14", want: dateWindowSpec{Mode: "days", Days: 14}},
		{name: "trimmed numeric days", input: " 30 ", want: dateWindowSpec{Mode: "days", Days: 30}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseExplicitPeriodValue(tc.input)
			if err != nil {
				t.Fatalf("parseExplicitPeriodValue(%q) returned error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("parseExplicitPeriodValue(%q) = %+v, want %+v", tc.input, got, tc.want)
			}
		})
	}
}

func TestParseExplicitPeriodValueRejectsInvalidValues(t *testing.T) {
	for _, input := range []string{"", "banana", "0", "-3"} {
		t.Run(input, func(t *testing.T) {
			if _, err := parseExplicitPeriodValue(input); err == nil {
				t.Fatalf("parseExplicitPeriodValue(%q) should fail", input)
			}
		})
	}
}
