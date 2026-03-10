package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
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
)

type app struct {
	basePath    string
	redirectURL string
	client      *http.Client
	transport   *http.Transport
}

func main() {
	port := envOrDefault("PORT", defaultPort)
	basePath := requiredEnv("BASE_PATH")
	redirectURL := requiredEnv("REDIRECT_URL")

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
		basePath:    strings.TrimRight(basePath, "/"),
		redirectURL: redirectURL,
		client: &http.Client{
			Timeout:   upstreamReqTimeout,
			Transport: transport,
		},
		transport: transport,
	}

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

	if r.URL.Path == defaultPath {
		r.URL.Path += "index.html"
	}

	targetURL, err := url.Parse(fmt.Sprintf("%s/%s", a.basePath, subdomain))
	if err != nil {
		log.Printf("Error parsing target URL: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	ok, err := a.upstreamExists(r.Context(), targetURL)
	if err != nil {
		log.Printf("Error checking upstream target %s: %v", targetURL, err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	if !ok {
		http.Redirect(w, r, a.redirectURL, http.StatusSeeOther)
		return
	}

	log.Printf("Proxying to %s", targetURL)

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.Transport = a.transport
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, proxyErr error) {
		log.Printf("Proxy error for %s: %v", targetURL, proxyErr)
		rw.WriteHeader(http.StatusBadGateway)
	}

	r.Host = targetURL.Host
	proxy.ServeHTTP(w, r)
}

func (a *app) upstreamExists(ctx context.Context, targetURL *url.URL) (bool, error) {
	indexURL := targetURL.JoinPath("index.html")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, indexURL.String(), nil)
	if err != nil {
		return false, err
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() {
		if _, copyErr := io.Copy(io.Discard, resp.Body); copyErr != nil {
			log.Printf("Error draining upstream response body for %s: %v", indexURL, copyErr)
		}
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Printf("Error closing upstream response body for %s: %v", indexURL, closeErr)
		}
	}()

	return resp.StatusCode == http.StatusOK, nil
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

func subdomainFromHost(host string) (string, error) {
	hostname := host
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		hostname = parsedHost
	}

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

	return subdomain, nil
}
