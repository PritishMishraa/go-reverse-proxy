package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSubdomainFromHost(t *testing.T) {
	tests := []struct {
		name      string
		host      string
		want      string
		wantError bool
	}{
		{name: "subdomain", host: "app.example.com", want: "app"},
		{name: "subdomain with port", host: "app.example.com:8080", want: "app"},
		{name: "www subdomain", host: "www.app.example.com", want: "app"},
		{name: "uppercase normalizes", host: "App.Example.Com", want: "app"},
		{name: "trailing dot", host: "app.example.com.", want: "app"},
		{name: "no subdomain", host: "localhost", wantError: true},
		{name: "invalid underscore", host: "bad_name.example.com", wantError: true},
		{name: "leading hyphen", host: "-bad.example.com", wantError: true},
		{name: "trailing hyphen", host: "bad-.example.com", wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := subdomainFromHost(tt.host)
			if tt.wantError {
				if err == nil {
					t.Fatalf("subdomainFromHost(%q) error = nil, want error", tt.host)
				}
				return
			}
			if err != nil {
				t.Fatalf("subdomainFromHost(%q) error = %v", tt.host, err)
			}
			if got != tt.want {
				t.Fatalf("subdomainFromHost(%q) = %q, want %q", tt.host, got, tt.want)
			}
		})
	}
}

func TestTargetURLJoinsSubdomain(t *testing.T) {
	baseURL := mustParseTestURL(t, "https://static.example.com/sites/")
	a := &app{baseURL: baseURL}

	got := a.targetURL("demo").String()
	want := "https://static.example.com/sites/demo"
	if got != want {
		t.Fatalf("targetURL = %q, want %q", got, want)
	}
}

func TestTargetURLAddsLeadingSlashWhenBaseHasNoPath(t *testing.T) {
	baseURL := mustParseTestURL(t, "https://simple-host.s3.ap-south-1.amazonaws.com")
	a := &app{baseURL: baseURL}

	got := a.targetURL("miniytm")
	if got.String() != "https://simple-host.s3.ap-south-1.amazonaws.com/miniytm" {
		t.Fatalf("targetURL = %q, want S3 object prefix URL", got.String())
	}
	if got.Path != "/miniytm" {
		t.Fatalf("targetURL path = %q, want /miniytm", got.Path)
	}
}

func TestJoinURLPathKeepsLeadingSlashForS3IndexObject(t *testing.T) {
	baseURL := mustParseTestURL(t, "https://simple-host.s3.ap-south-1.amazonaws.com/miniytm")
	requestURL := mustParseTestURL(t, "/index.html")

	path, rawPath := joinURLPath(baseURL, requestURL)
	if path != "/miniytm/index.html" {
		t.Fatalf("path = %q, want /miniytm/index.html", path)
	}
	if rawPath != "" {
		t.Fatalf("rawPath = %q, want empty", rawPath)
	}
}

func TestUpstreamExistsUsesHead(t *testing.T) {
	var methods []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.URL.Path != "/demo/index.html" {
			t.Fatalf("path = %q, want /demo/index.html", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	a := &app{client: upstream.Client()}
	ok, err := a.upstreamExists(context.Background(), mustParseTestURL(t, upstream.URL+"/demo"))
	if err != nil {
		t.Fatalf("upstreamExists error = %v", err)
	}
	if !ok {
		t.Fatal("upstreamExists = false, want true")
	}
	if !reflect.DeepEqual(methods, []string{http.MethodHead}) {
		t.Fatalf("methods = %v, want [HEAD]", methods)
	}
}

func TestUpstreamExistsFallsBackToGet(t *testing.T) {
	var methods []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	a := &app{client: upstream.Client()}
	ok, err := a.upstreamExists(context.Background(), mustParseTestURL(t, upstream.URL+"/demo"))
	if err != nil {
		t.Fatalf("upstreamExists error = %v", err)
	}
	if !ok {
		t.Fatal("upstreamExists = false, want true")
	}
	if !reflect.DeepEqual(methods, []string{http.MethodHead, http.MethodGet}) {
		t.Fatalf("methods = %v, want [HEAD GET]", methods)
	}
}

func TestHandleRequestProxiesWithReusableProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/sites/demo/index.html" {
			t.Fatalf("path = %q, want /sites/demo/index.html", r.URL.Path)
		}
		_, _ = w.Write([]byte("proxied"))
	}))
	defer upstream.Close()

	baseURL := mustParseTestURL(t, upstream.URL+"/sites")
	redirectURL := mustParseTestURL(t, "https://example.com/missing")
	a := &app{
		baseURL:     baseURL,
		redirectURL: redirectURL,
		client:      upstream.Client(),
		cache:       newExistenceCache(),
		now:         time.Now,
	}
	a.proxy = newReverseProxy(upstream.Client().Transport)

	req := httptest.NewRequest(http.MethodGet, "http://demo.example.com/", nil)
	req.Host = "demo.example.com"
	rec := httptest.NewRecorder()

	a.handleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != "proxied" {
		t.Fatalf("body = %q, want proxied", rec.Body.String())
	}
}

func TestHandleRequestRewritesRootToS3IndexObject(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://simple-host.s3.ap-south-1.amazonaws.com/miniytm/index.html" {
			t.Fatalf("upstream URL = %q, want S3 index object URL", r.URL.String())
		}
		if r.Host != "simple-host.s3.ap-south-1.amazonaws.com" {
			t.Fatalf("upstream Host = %q, want S3 host", r.Host)
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("proxied")),
			Request:    r,
		}, nil
	})
	a := &app{
		baseURL:     mustParseTestURL(t, "https://simple-host.s3.ap-south-1.amazonaws.com"),
		redirectURL: mustParseTestURL(t, "https://smoll-host.vercel.app/"),
		client:      &http.Client{Transport: transport},
		cache:       newExistenceCache(),
		now:         time.Now,
	}
	a.proxy = newReverseProxy(transport)
	a.cache.set("miniytm", true, time.Now().Add(time.Minute))

	req := httptest.NewRequest(http.MethodGet, "http://miniytm.pritish.in/", nil)
	req.Host = "miniytm.pritish.in"
	rec := httptest.NewRecorder()

	a.handleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestHandleRequestCachesRootExistenceCheck(t *testing.T) {
	var checks int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			checks++
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write([]byte("proxied"))
	}))
	defer upstream.Close()

	a := newTestApp(t, upstream, "https://example.com/missing")

	for range 2 {
		req := httptest.NewRequest(http.MethodGet, "http://demo.example.com/", nil)
		req.Host = "demo.example.com"
		rec := httptest.NewRecorder()

		a.handleRequest(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
	}

	if checks != 1 {
		t.Fatalf("existence checks = %d, want 1", checks)
	}
}

func TestHandleRequestCachesMissingRoot(t *testing.T) {
	var checks int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			checks++
			w.WriteHeader(http.StatusNotFound)
			return
		}
		t.Fatalf("unexpected proxy request: %s %s", r.Method, r.URL.Path)
	}))
	defer upstream.Close()

	a := newTestApp(t, upstream, "https://example.com/missing")

	for range 2 {
		req := httptest.NewRequest(http.MethodGet, "http://missing.example.com/", nil)
		req.Host = "missing.example.com"
		rec := httptest.NewRecorder()

		a.handleRequest(rec, req)

		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
		}
	}

	if checks != 1 {
		t.Fatalf("existence checks = %d, want 1", checks)
	}
}

func TestHandleRequestSkipsExistenceCheckForAssets(t *testing.T) {
	var checks int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			checks++
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path != "/sites/demo/main.css" {
			t.Fatalf("path = %q, want /sites/demo/main.css", r.URL.Path)
		}
		_, _ = w.Write([]byte("asset"))
	}))
	defer upstream.Close()

	a := newTestApp(t, upstream, "https://example.com/missing")
	req := httptest.NewRequest(http.MethodGet, "http://demo.example.com/main.css", nil)
	req.Host = "demo.example.com"
	rec := httptest.NewRecorder()

	a.handleRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if checks != 0 {
		t.Fatalf("existence checks = %d, want 0", checks)
	}
}

func TestCachedUpstreamExistsRefreshesAfterExpiration(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	var checks int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	a := &app{
		client: upstream.Client(),
		cache:  newExistenceCache(),
		now: func() time.Time {
			return now
		},
	}
	targetURL := mustParseTestURL(t, upstream.URL+"/demo")

	if ok, err := a.cachedUpstreamExists(context.Background(), "demo", targetURL); err != nil || !ok {
		t.Fatalf("cachedUpstreamExists first = %v, %v; want true, nil", ok, err)
	}
	if ok, err := a.cachedUpstreamExists(context.Background(), "demo", targetURL); err != nil || !ok {
		t.Fatalf("cachedUpstreamExists cached = %v, %v; want true, nil", ok, err)
	}
	now = now.Add(existsCacheTTL)
	if ok, err := a.cachedUpstreamExists(context.Background(), "demo", targetURL); err != nil || !ok {
		t.Fatalf("cachedUpstreamExists expired = %v, %v; want true, nil", ok, err)
	}

	if checks != 2 {
		t.Fatalf("existence checks = %d, want 2", checks)
	}
}

func newTestApp(t *testing.T, upstream *httptest.Server, redirectURL string) *app {
	t.Helper()

	a := &app{
		baseURL:     mustParseTestURL(t, upstream.URL+"/sites"),
		redirectURL: mustParseTestURL(t, redirectURL),
		client:      upstream.Client(),
		cache:       newExistenceCache(),
		now:         time.Now,
	}
	a.proxy = newReverseProxy(upstream.Client().Transport)

	return a
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func mustParseTestURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v", rawURL, err)
	}

	return parsedURL
}
