package authhelper

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestProjectCookiesPreservesOrderFieldsAndIndependentSlice(t *testing.T) {
	inside := fixtureCookies("synthetic")
	inside[0].Domain = ".GOOGLE.COM"
	inside = append(inside, aistudio.StateCookie{Name: "PREF", Value: "extra", Domain: "AiStudio.Google.Com", Path: "/fixture", Expires: 123.5, HTTPOnly: true, Secure: true, SameSite: "Strict"})
	outside := aistudio.StateCookie{Name: "OTHER", Value: "outside", Domain: ".youtube.com", Path: "/", Expires: -1}
	full := []aistudio.StateCookie{outside, inside[0], inside[1], outside, inside[2], inside[3]}
	before := append([]aistudio.StateCookie(nil), full...)
	got, err := projectCookies(full, nil)
	if err != nil || !reflect.DeepEqual(got, inside) || !reflect.DeepEqual(full, before) {
		t.Fatal("selection changed order/fields/input", err)
	}
	got[0].Value = "mutated-only-output"
	got = append(got, outside)
	if !reflect.DeepEqual(full, before) {
		t.Fatal("projection aliases complete authority")
	}
	if ValidateCookies(full, nil) == nil {
		t.Fatal("strict receiver was turned into a filter")
	}
}

func TestProjectCookiesDomainPolicyRetainsOriginalMatching(t *testing.T) {
	for _, tc := range []struct {
		name, domain string
		domains      []string
		selected     bool
	}{
		{"default-exact", "google.com", nil, true},
		{"empty-default", ".google.com", []string{}, true},
		{"case", ".GoOgLe.CoM", []string{".GOOGLE.COM"}, true},
		{"subdomain", "accounts.google.com", nil, true},
		{"two-leading-dots-existing-suffix-rule", "..google.com", nil, true},
		{"leading-space-not-trimmed", " google.com", nil, false},
		{"trailing-space-not-trimmed", "google.com ", nil, false},
		{"trailing-dot-not-trimmed", "google.com.", nil, false},
		{"suffix-attack", "google.com.evil.invalid", nil, false},
		{"prefix-attack", "evilgoogle.com", nil, false},
		{"youtube-default-denied", ".youtube.com", nil, false},
		{"fixed-additional-rule", ".YOUTUBE.COM", []string{"google.com", ".youtube.com"}, true},
		{"fixed-subdomain-rule", "sub.youtube.com", []string{"google.com", "youtube.com"}, true},
		{"rule-leading-space-not-trimmed", "youtube.com", []string{"google.com", " youtube.com"}, false},
		{"rule-one-leading-dot", "youtube.com", []string{"google.com", "..youtube.com"}, false},
		{"empty-rule-does-not-broaden", "youtube.com", []string{"google.com", ""}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := fixtureCookies("synthetic")
			candidate := aistudio.StateCookie{Name: "EXTRA", Value: "synthetic-extra", Domain: tc.domain, Path: "/", Expires: -1}
			full := append(base, candidate)
			got, err := projectCookies(full, tc.domains)
			want := base[:3]
			if tc.selected {
				want = full
			}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatal("domain matching semantics changed", err)
			}
			if (ValidateCookies(full, tc.domains) == nil) != tc.selected {
				t.Fatal("strict policy and selection disagree")
			}
		})
	}
}

func TestProjectCookiesRejectsEveryInDomainInvalidEntry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]aistudio.StateCookie) []aistudio.StateCookie
	}{
		{"empty", func([]aistudio.StateCookie) []aistudio.StateCookie { return nil }},
		{"outside-only", func(c []aistudio.StateCookie) []aistudio.StateCookie {
			for i := range c {
				c[i].Domain = ".youtube.com"
			}
			return c
		}},
		{"missing-signer", func(c []aistudio.StateCookie) []aistudio.StateCookie { return c[1:] }},
		{"signer-empty", func(c []aistudio.StateCookie) []aistudio.StateCookie { c[0].Value = ""; return c }},
		{"signer-expired", func(c []aistudio.StateCookie) []aistudio.StateCookie {
			c[0].Expires = float64(time.Now().Add(-time.Hour).Unix())
			return c
		}},
		{"signer-wrong-path", func(c []aistudio.StateCookie) []aistudio.StateCookie { c[0].Path = "/not-at-origin"; return c }},
		{"duplicate", func(c []aistudio.StateCookie) []aistudio.StateCookie { return append(c, c[0]) }},
		{"duplicate-normalized-domain", func(c []aistudio.StateCookie) []aistudio.StateCookie {
			extra := c[0]
			extra.Domain = "GOOGLE.COM"
			return append(c, extra)
		}},
		{"partition", func(c []aistudio.StateCookie) []aistudio.StateCookie { c[0].PartitionKey = "synthetic"; return c }},
		{"empty-name", func(c []aistudio.StateCookie) []aistudio.StateCookie { c[0].Name = ""; return c }},
		{"long-name", func(c []aistudio.StateCookie) []aistudio.StateCookie { c[0].Name = strings.Repeat("x", 257); return c }},
		{"long-value", func(c []aistudio.StateCookie) []aistudio.StateCookie {
			c[0].Value = strings.Repeat("x", 16385)
			return c
		}},
		{"illegal-name", func(c []aistudio.StateCookie) []aistudio.StateCookie { c[0].Name = "bad;name"; return c }},
		{"newline-name", func(c []aistudio.StateCookie) []aistudio.StateCookie { c[0].Name = "bad\nname"; return c }},
		{"newline-value", func(c []aistudio.StateCookie) []aistudio.StateCookie { c[0].Value = "bad\r\nvalue"; return c }},
		{"nul-value", func(c []aistudio.StateCookie) []aistudio.StateCookie { c[0].Value = "bad\x00value"; return c }},
		{"bad-path", func(c []aistudio.StateCookie) []aistudio.StateCookie { c[0].Path = "relative"; return c }},
		{"bad-samesite", func(c []aistudio.StateCookie) []aistudio.StateCookie { c[0].SameSite = "invalid"; return c }},
		{"nan", func(c []aistudio.StateCookie) []aistudio.StateCookie { c[0].Expires = math.NaN(); return c }},
		{"positive-inf", func(c []aistudio.StateCookie) []aistudio.StateCookie { c[0].Expires = math.Inf(1); return c }},
		{"negative-inf", func(c []aistudio.StateCookie) []aistudio.StateCookie { c[0].Expires = math.Inf(-1); return c }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			full := tc.mutate(fixtureCookies("synthetic"))
			got, err := projectCookies(full, nil)
			if safeCode(err) != "COOKIE_POLICY_FAILURE" || got != nil {
				t.Fatal("invalid in-domain entry was removed/repaired/accepted", err)
			}
		})
	}
}

func TestProjectCookiesLimitsOnlySelectedSubset(t *testing.T) {
	full := fixtureCookies("synthetic")
	for i := range 125 {
		full = append(full, aistudio.StateCookie{Name: fmt.Sprintf("EXTRA_%03d", i), Value: "synthetic", Domain: ".google.com", Path: "/", Expires: -1})
	}
	selected := append([]aistudio.StateCookie(nil), full...)
	for i := range 140 {
		full = append(full, aistudio.StateCookie{Name: fmt.Sprintf("OTHER_%03d", i), Value: "synthetic", Domain: ".youtube.com", Path: "/", Expires: -1})
	}
	got, err := projectCookies(full, nil)
	if err != nil || !reflect.DeepEqual(got, selected) || len(full) != 268 {
		t.Fatal("full authority was capped or subset truncated", err)
	}
	full = append(full, aistudio.StateCookie{Name: "TOO_MANY", Domain: ".google.com", Path: "/", Expires: -1})
	if got, err := projectCookies(full, nil); got != nil || safeCode(err) != "COOKIE_POLICY_FAILURE" {
		t.Fatal("oversized projection accepted", err)
	}
}

func TestProjectCookiesDoesNotCleanExpiredAuxiliaryOrInspectUnselectedFields(t *testing.T) {
	cookies := fixtureCookies("synthetic")
	auxiliary := aistudio.StateCookie{Name: "EXPIRED_AUX", Domain: ".google.com", Path: "/", Expires: 1}
	cookies = append(cookies, auxiliary)
	// These fields are deliberately outside the RPC domain. The complete native
	// state still has to satisfy its own writer/schema; projection is not that writer.
	cookies = append(cookies, aistudio.StateCookie{Domain: ".youtube.com", PartitionKey: "synthetic", Expires: math.NaN()})
	got, err := projectCookies(cookies, nil)
	if err != nil || !reflect.DeepEqual(got, cookies[:4]) {
		t.Fatal("projection added expiration cleaning or audited unselected RPC fields", err)
	}
}
