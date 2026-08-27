package server

import "testing"

// TestReviseLocale exercises reviseLocale directly: previously
// RevisedLocaleID simply echoed the request's LocaleID, which misreported
// an unsupported locale as though it had been honored. reviseLocale is
// the single place that must now get this right for every operation
// (via opContext.replyBase).
func TestReviseLocale(t *testing.T) {
	cases := []struct {
		name      string
		requested string
		supported []string
		want      string
	}{
		{"no supported locales at all", "en-US", nil, ""},
		{"empty supported slice", "en-US", []string{}, ""},
		{"no locale requested falls back to first supported", "", []string{"en-US", "de-DE"}, "en-US"},
		{"exact match", "de-DE", []string{"en-US", "de-DE"}, "de-DE"},
		{"case-insensitive match returns canonical spelling", "de-de", []string{"en-US", "de-DE"}, "de-DE"},
		{"case-insensitive match, request uppercase", "EN-us", []string{"en-US"}, "en-US"},
		{"unsupported locale falls back to first supported", "fr-FR", []string{"en-US", "de-DE"}, "en-US"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reviseLocale(tc.requested, tc.supported); got != tc.want {
				t.Fatalf("reviseLocale(%q, %v) = %q, want %q", tc.requested, tc.supported, got, tc.want)
			}
		})
	}
}
