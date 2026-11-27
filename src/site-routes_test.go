package main

import (
	"crypto/tls"
	"encoding/json"
	"gopkg.in/yaml.v3"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestSiteRouteIsolationAcrossHTTPVersions(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		app := scopeTestApp(t)
		cfg := app.current.Load().config
		cfg.Scope = deviceScope{Mode: "all"}
		cfg.Sites = []siteConfig{
			{Domain: "b.a.com", Routes: []customRoute{{Path: "/c", JSON: `{"site":"b"}`}}},
			{Domain: "h.g.com", Routes: []customRoute{{Path: "/c", JSON: `{"site":"h"}`}, {Path: "/stop", ResponseMode: "close"}}},
			{Domain: "*.a.com", Routes: []customRoute{{Path: "*", JSON: `{"site":"wild"}`}}},
			{Domain: "empty.a.com", Routes: []customRoute{}},
		}
		if err := cfg.validate(); err != nil {
			t.Fatal(err)
		}
		app.current.Store(app.runtimeSettings(cfg, &tls.Certificate{}))
		srv := httptest.NewUnstartedServer(http.HandlerFunc(app.publicSite))
		srv.EnableHTTP2 = h2
		srv.Config.ConnContext = requestConnectionContext
		srv.StartTLS()
		client := srv.Client()
		for _, tc := range []struct {
			host, path, body string
			code             int
		}{
			{"b.a.com", "/c", `{"site":"b"}`, 200}, {"h.g.com", "/c", `{"site":"h"}`, 200},
			{"b.a.com", "/d", "", 404}, {"b.a.com", "/status", "", 404}, {"empty.a.com", "/c", "", 404},
			{"x.y.a.com", "/anything/deep", `{"site":"wild"}`, 200}, {"e.f.com", "/c", "", 404},
			{"B.A.COM:443", "/c", `{"site":"b"}`, 200},
		} {
			req, _ := http.NewRequest("GET", srv.URL+tc.path, nil)
			req.Host = tc.host
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			wantProto := 1
			if h2 {
				wantProto = 2
			}
			if res.ProtoMajor != wantProto || res.StatusCode != tc.code || (tc.body != "" && string(body) != tc.body) {
				t.Fatalf("%+v: %d %s", tc, res.StatusCode, body)
			}
		}
		req, _ := http.NewRequest("GET", srv.URL+"/stop", nil)
		req.Host = "h.g.com"
		res, err := client.Do(req)
		if res != nil {
			res.Body.Close()
		}
		if err == nil {
			t.Fatal("site close mode not applied")
		}
		// Same path on another domain cannot invoke the close rule.
		req, _ = http.NewRequest("GET", srv.URL+"/stop", nil)
		req.Host = "b.a.com"
		res, err = client.Do(req)
		if err != nil || res.StatusCode != 404 {
			t.Fatal("close rule leaked")
		}
		res.Body.Close()
		client.CloseIdleConnections()
		srv.Close()
	}
}

func TestSiteRouteYAMLSchema(t *testing.T) {
	good := []byte("version: 1\nh2_sites:\n  - domain: b.a.com\n    h2_routes:\n      - path: /c\n        json: '{}'\n  - domain: h.g.com\n    h2_routes:\n      - path: /c\n        response_mode: close\n")
	cfg, err := decodeConfig(good)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Sites[0].Routes = append(cfg.Sites[0].Routes, cfg.Sites[0].Routes[0])
	if cfg.validate() == nil {
		t.Fatal("duplicate within site accepted")
	}
	for _, tail := range []string{"h2_routes: []\n", "h2_routes:\n  - path: /old\n    json: '{}'\n"} {
		if _, err := decodeConfig(append(good, []byte(tail)...)); err == nil {
			t.Fatal("global routes accepted")
		}
	}
	cfg, _ = decodeConfig(good)
	data, _ := yaml.Marshal(cfg)
	if strings.Contains(string(data), "\nh2_routes:") {
		t.Fatal("exported global routes")
	}
}

func TestSiteEditorRoutesStayWithTheirSite(t *testing.T) {
	app := profileTestApp(t)
	active, _ := app.activeProfile()
	doc, _, _ := app.readProfileDocument(active)
	sites := []editorSiteUpdate{
		{Domain: "b.a.com", Routes: []editorRouteUpdate{{Path: "/c", JSON: `{"n":9007199254740993}`}}},
		{Domain: "h.g.com", Routes: []editorRouteUpdate{{Path: "/c", JSON: `{"n":2}`}}},
	}
	rules := []dnsRule{}
	u := editorUpdate{Revision: doc.Revision, DNSRules: &rules, Sites: &sites}
	saved, err := app.editProfileDocument(profileAction{Action: "write_editor", Name: active, Revision: doc.Revision, Editor: &u})
	if err != nil {
		t.Fatal(err)
	}
	// Reorder and rename one site while referencing the same route path in each.
	sites = []editorSiteUpdate{{Domain: "h.g.com", Routes: []editorRouteUpdate{{Path: "/c", Source: "/c"}}}, {Domain: "renamed.a.com", Source: "b.a.com", Routes: []editorRouteUpdate{{Path: "/c", Source: "/c"}}}}
	u.Revision = saved.Revision
	u.Sites = &sites
	_, err = app.editProfileDocument(profileAction{Action: "write_editor", Name: active, Revision: saved.Revision, Editor: &u})
	if err != nil {
		t.Fatal(err)
	}
	doc, cfg, err := app.readProfileDocument(active)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sites[0].Routes[0].JSON != `{"n":2}` || cfg.Sites[1].Routes[0].JSON != `{"n":9007199254740993}` {
		t.Fatal("site route content crossed")
	}
	encoded, _ := json.Marshal(doc.Editor)
	var shape map[string]json.RawMessage
	json.Unmarshal(encoded, &shape)
	if _, ok := shape["h2_routes"]; ok {
		t.Fatal("editor exports global routes")
	}
	if app.current.Load().Sites[0].Domain != "site.home.arpa" {
		t.Fatal("save applied draft")
	}
}

func TestBundledSiteRouteExamples(t *testing.T) {
	for _, name := range []string{"config.example.yaml", "config.multi.example.yaml", "仅监控.yaml"} {
		data, err := os.ReadFile("../files/" + name)
		if os.IsNotExist(err) {
			t.Skip("source examples unavailable")
		}
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := decodeConfig(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !cfg.MonitorOnly && len(cfg.Sites[0].Routes) == 0 {
			t.Fatal("example lacks site routes")
		}
	}
}
