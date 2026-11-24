package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSingleSegmentWildcardResponses(t *testing.T) {
	cfg := config{Version: 1, Site: siteConfig{Domain: "site.home.arpa"}, Routes: []customRoute{
		{Path: "/*/*/a", JSON: `{"rule":"broad"}`},
		{Path: "/b/*/a", JSON: `{"rule":"segment"}`},
		{Path: "/b/fixed/a", JSON: `{"rule":"exact"}`},
		{Path: "/a", JSON: `{"rule":"plain"}`},
	}}
	app := testApp(t, cfg)
	for _, tc := range []struct {
		path, rule string
		status     int
	}{
		{"/b/123/a", "segment", 200}, {"/b/abc/a", "segment", 200},
		{"/b/abc/a?q=test", "segment", 200}, {"/b/fixed/a", "exact", 200},
		{"/a", "plain", 200}, {"/other/value/a", "broad", 200},
		{"/b/a", "", 404}, {"/b//a", "", 404}, {"/b/x/y/a", "", 404},
		{"/b/x/a/", "", 404}, {"/b/x/A", "", 404}, {"/prefix/b/x/a", "", 404},
		{"/b/x%2Fy/a", "", 404},
	} {
		w := httptest.NewRecorder()
		app.publicSite(w, httptest.NewRequest("GET", "https://site.home.arpa"+tc.path, nil))
		if w.Code != tc.status || (tc.rule != "" && w.Body.String() != `{"rule":"`+tc.rule+`"}`) {
			t.Errorf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	// With no competing wildcard, an existing exact route never acquires suffix matching.
	if _, ok := matchingRoute([]customRoute{{Path: "/a"}}, "/b/c/a"); ok {
		t.Fatal("exact route became a suffix match")
	}
}

func TestWildcardPrecedenceAndDiagnostics(t *testing.T) {
	routes := []customRoute{{Path: "/b/*/*"}, {Path: "/b/*/a"}, {Path: "/b/c/a"}, {Path: "/*/c/a"}, {Path: "/b/fsd-check-0/a"}}
	for _, path := range []string{"/b/c/a", "/b/fsd-check-0/a"} {
		got, ok := matchingRoute(routes, path)
		if !ok || got.Path != path {
			t.Fatalf("exact route lost: %+v", got)
		}
	}
	// Equal-specificity wildcard rules use their original order.
	ties := []customRoute{{Path: "/b/*/a"}, {Path: "/*/c/a"}}
	got, _ := matchingRoute(ties, "/b/c/a")
	if got.Path != ties[0].Path {
		t.Fatal("tie precedence changed")
	}
	for _, route := range routes {
		path := diagnosticRoutePath(route.Path, routes)
		if strings.Contains(path, "*") {
			t.Fatal("diagnostic sent a literal star")
		}
		got, ok := matchingRoute(routes, path)
		if !ok || got.Path != route.Path {
			t.Fatalf("probe %s hit %s instead of %s", path, got.Path, route.Path)
		}
	}
	if routeMatches("/b/*/a", "/b//a") || routeMatches("/b/*/a", "/b/x/y/a") {
		t.Fatal("wildcard crossed segment boundary")
	}
	if !routeMatches("/b/*/a/", "/b/x/a/") || routeMatches("/b/*/a/", "/b/x/a") {
		t.Fatal("trailing slash was normalized")
	}
}

func TestWildcardPatternValidation(t *testing.T) {
	for _, path := range []string{"/b/*/a", "/*", "/*/*", "/b/*/a/", "/a"} {
		cfg := config{Version: 1, Site: siteConfig{Domain: "site.home.arpa"}, Routes: []customRoute{{Path: path, JSON: `{}`}}}
		if err := cfg.validate(); err != nil {
			t.Errorf("valid pattern %q: %v", path, err)
		}
	}
	for _, path := range []string{"/b/**/a", "/b/pre*/a", "/b/*post/a", "/b/a*b/a", "/b//a", "/b/%2a/a"} {
		cfg := config{Version: 1, Site: siteConfig{Domain: "site.home.arpa"}, Routes: []customRoute{{Path: path, JSON: `{}`}}}
		if err := cfg.validate(); err == nil {
			t.Errorf("invalid pattern accepted: %q", path)
		}
	}
}
