package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	defaultPort          = "8080"
	defaultPath          = "/"
	serverReadTimeout    = 5 * time.Second
	serverReadHeaderTO   = 5 * time.Second
	serverWriteTimeout   = 30 * time.Second
	serverIdleTimeout    = 60 * time.Second
	upstreamDialTimeout  = 5 * time.Second
	upstreamKeepAlive    = 30 * time.Second
	upstreamTLSHandshake = 5 * time.Second
	upstreamRespHeaderTO = 10 * time.Second
	upstreamExpectContTO = 1 * time.Second
	upstreamReqTimeout   = 15 * time.Second
	existsCacheTTL       = 5 * time.Minute
	missingCacheTTL      = 30 * time.Second
)

type contextKey struct{}

var targetURLContextKey contextKey

type app struct {
	baseURL     *url.URL
	redirectURL *url.URL
	client      *http.Client
	proxy       *httputil.ReverseProxy
	cache       *existenceCache
	now         func() time.Time
}

type existenceCache struct {
	mu      sync.RWMutex
	entries map[string]existenceCacheEntry
}

type existenceCacheEntry struct {
	exists    bool
	expiresAt time.Time
}

func main() {
	port := envOrDefault("PORT", defaultPort)
	baseURL := mustParseURL("BASE_PATH")
	redirectURL := mustParseURL("REDIRECT_URL")

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   upstreamDialTimeout,
			KeepAlive: upstreamKeepAlive,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   upstreamTLSHandshake,
		ExpectContinueTimeout: upstreamExpectContTO,
		ResponseHeaderTimeout: upstreamRespHeaderTO,
	}

	application := &app{
		baseURL:     baseURL,
		redirectURL: redirectURL,
		client: &http.Client{
			Timeout:   upstreamReqTimeout,
			Transport: transport,
		},
		cache: newExistenceCache(),
		now:   time.Now,
	}
	application.proxy = newReverseProxy(transport)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthCheckHandler)
	mux.HandleFunc("/", application.handleRequest)

	server := &http.Server{
		Addr:              "0.0.0.0:" + port,
		Handler:           mux,
		ReadTimeout:       serverReadTimeout,
		ReadHeaderTimeout: serverReadHeaderTO,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
	}

	log.Printf("Reverse Proxy Running on %s", port)
	log.Fatal(server.ListenAndServe())
}

func newReverseProxy(transport http.RoundTripper) *httputil.ReverseProxy {
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Director: func(req *http.Request) {
			targetURL, ok := req.Context().Value(targetURLContextKey).(*url.URL)
			if !ok || targetURL == nil {
				return
			}

			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.URL.Path, req.URL.RawPath = joinURLPath(targetURL, req.URL)
			if targetURL.RawQuery == "" || req.URL.RawQuery == "" {
				req.URL.RawQuery = targetURL.RawQuery + req.URL.RawQuery
			} else {
				req.URL.RawQuery = targetURL.RawQuery + "&" + req.URL.RawQuery
			}
			req.Host = targetURL.Host
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, proxyErr error) {
			log.Printf("Proxy error for %s: %v", req.URL.String(), proxyErr)
			rw.WriteHeader(http.StatusBadGateway)
		},
	}

	return proxy
}

func (a *app) handleRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	subdomain, err := subdomainFromHost(r.Host)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	log.Printf("Request for hostname: %s", r.Host)
	log.Printf("Request for subdomain: %s", subdomain)

	checkExists := r.URL.Path == defaultPath
	if checkExists {
		r.URL.Path += "index.html"
	}

	targetURL := a.targetURL(subdomain)

	if checkExists {
		ok, err := a.cachedUpstreamExists(r.Context(), subdomain, targetURL)
		if err != nil {
			log.Printf("Error checking upstream target %s: %v", targetURL, err)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
			return
		}
		if !ok {
			http.Redirect(w, r, a.redirectURL.String(), http.StatusSeeOther)
			return
		}
	}

	log.Printf("Proxying to %s", targetURL)

	ctx := context.WithValue(r.Context(), targetURLContextKey, targetURL)
	a.proxy.ServeHTTP(w, r.WithContext(ctx))
}

func (a *app) targetURL(subdomain string) *url.URL {
	return a.baseURL.JoinPath(subdomain)
}

func (a *app) cachedUpstreamExists(ctx context.Context, key string, targetURL *url.URL) (bool, error) {
	if exists, ok := a.cache.get(key, a.now()); ok {
		return exists, nil
	}

	exists, err := a.upstreamExists(ctx, targetURL)
	if err != nil {
		return false, err
	}

	ttl := missingCacheTTL
	if exists {
		ttl = existsCacheTTL
	}
	a.cache.set(key, exists, a.now().Add(ttl))

	return exists, nil
}

func (a *app) upstreamExists(ctx context.Context, targetURL *url.URL) (bool, error) {
	indexURL := targetURL.JoinPath("index.html")
	resp, err := a.doUpstreamExistsRequest(ctx, http.MethodHead, indexURL)
	if err != nil {
		return false, err
	}

	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		closeResponseBody(resp, indexURL)
		resp, err = a.doUpstreamExistsRequest(ctx, http.MethodGet, indexURL)
		if err != nil {
			return false, err
		}
	}
	defer closeResponseBody(resp, indexURL)

	return resp.StatusCode == http.StatusOK, nil
}

func (a *app) doUpstreamExistsRequest(ctx context.Context, method string, targetURL *url.URL) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, targetURL.String(), nil)
	if err != nil {
		return nil, err
	}

	return a.client.Do(req)
}

func closeResponseBody(resp *http.Response, targetURL *url.URL) {
	if resp == nil || resp.Body == nil {
		return
	}
	if _, copyErr := io.Copy(io.Discard, resp.Body); copyErr != nil {
		log.Printf("Error draining upstream response body for %s: %v", targetURL, copyErr)
	}
	if closeErr := resp.Body.Close(); closeErr != nil {
		log.Printf("Error closing upstream response body for %s: %v", targetURL, closeErr)
	}
}

func newExistenceCache() *existenceCache {
	return &existenceCache{
		entries: make(map[string]existenceCacheEntry),
	}
}

func (c *existenceCache) get(key string, now time.Time) (bool, bool) {
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return false, false
	}
	if !now.Before(entry.expiresAt) {
		c.mu.Lock()
		if current, ok := c.entries[key]; ok && !now.Before(current.expiresAt) {
			delete(c.entries, key)
		}
		c.mu.Unlock()
		return false, false
	}

	return entry.exists, true
}

func (c *existenceCache) set(key string, exists bool, expiresAt time.Time) {
	c.mu.Lock()
	c.entries[key] = existenceCacheEntry{
		exists:    exists,
		expiresAt: expiresAt,
	}
	c.mu.Unlock()
}

func healthCheckHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Server Healthy!"))
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	return value
}

func requiredEnv(key string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		log.Fatalf("%s is not set", key)
	}

	return value
}

func mustParseURL(envKey string) *url.URL {
	rawURL := requiredEnv(envKey)
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		log.Fatalf("%s is invalid: %v", envKey, err)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		log.Fatalf("%s must use http or https", envKey)
	}
	if parsedURL.Host == "" {
		log.Fatalf("%s must include a host", envKey)
	}

	return parsedURL
}

func subdomainFromHost(host string) (string, error) {
	hostname := host
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		hostname = parsedHost
	}
	hostname = strings.TrimSuffix(strings.ToLower(hostname), ".")

	parts := strings.Split(hostname, ".")
	if len(parts) < 2 {
		return "", errors.New("host does not include a subdomain")
	}

	subdomain := parts[0]
	if subdomain == "www" {
		if len(parts) < 3 {
			return "", errors.New("www host does not include a subdomain")
		}
		subdomain = parts[1]
	}

	if subdomain == "" {
		return "", errors.New("empty subdomain")
	}
	if !validDNSLabel(subdomain) {
		return "", errors.New("invalid subdomain")
	}

	return subdomain, nil
}

func validDNSLabel(label string) bool {
	if len(label) < 1 || len(label) > 63 {
		return false
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for _, r := range label {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '-' {
			continue
		}

		return false
	}

	return true
}

func joinURLPath(baseURL, requestURL *url.URL) (path, rawPath string) {
	if baseURL.RawPath == "" && requestURL.RawPath == "" {
		return singleJoiningSlash(baseURL.Path, requestURL.Path), ""
	}

	basePath := escapedPath(baseURL)
	requestPath := escapedPath(requestURL)
	joinedPath := singleJoiningSlash(basePath, requestPath)
	parsedURL, err := url.Parse(joinedPath)
	if err != nil {
		return singleJoiningSlash(baseURL.Path, requestURL.Path), ""
	}

	return parsedURL.Path, parsedURL.RawPath
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	default:
		return a + b
	}
}

func escapedPath(u *url.URL) string {
	if u.RawPath != "" {
		return u.EscapedPath()
	}

	return u.Path
}
