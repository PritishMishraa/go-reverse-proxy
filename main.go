package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
)

func main() {
	PORT := os.Getenv("PORT")
	if PORT == "" {
		PORT = "8080"
	}
	http.HandleFunc("/health", healthCheckHandler)
	http.HandleFunc("/", handleRequest)
	log.Printf("Reverse Proxy Running on %s", PORT)
	log.Fatal(http.ListenAndServe("0.0.0.0:"+PORT, nil))
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	BASE_PATH, _ := os.LookupEnv("BASE_PATH")

	if BASE_PATH == "" {
		log.Fatal("BASE_PATH is not set")
	}
	hostname := r.Host
	subdomain := strings.Split(hostname, ".")[0]
	log.Printf("Request for subdomain: %s", subdomain)

	defaultPath := "/"

	if r.URL.Path == defaultPath {
		r.URL.Path += "index.html"
	}

	// Custom Domain - DB Query

	resolvesTo := fmt.Sprintf("%s/%s", BASE_PATH, subdomain)
	target, err := url.Parse(resolvesTo)
	log.Printf("Proxying to %s", target)
	if err != nil {
		log.Printf("Error parsing target URL: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(target)

	r.Host = target.Host

	proxy.ServeHTTP(w, r)
}

func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Server Healthy!"))
}
