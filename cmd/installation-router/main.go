// The router selects a local controller by installation identity. Each
// controller independently verifies the original body signature and identity.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type repositoryRoute struct {
	installationID int64
	fullName       string
}

func installationRouter(routes map[int64]http.Handler, repositories map[repositoryRoute]http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/webhook" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		var event struct {
			Installation struct {
				ID int64 `json:"id"`
			} `json:"installation"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		upstream, found := routes[event.Installation.ID]
		if !found {
			http.Error(w, "installation not authorized", http.StatusForbidden)
			return
		}
		// Routing does not authorize provisioning: the selected controller must
		// verify the unchanged signature, installation and GitHub run identity.
		if selected, ok := repositories[repositoryRoute{event.Installation.ID, strings.ToLower(event.Repository.FullName)}]; ok {
			upstream = selected
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		upstream.ServeHTTP(w, r)
	})
}

func main() {
	routes := make(map[int64]http.Handler)
	for key, address := range map[string]string{"LEGACY_INSTALLATION_ID": "http://127.0.0.1:8080", "KADUPUL_INSTALLATION_ID": "http://127.0.0.1:8081"} {
		id, err := strconv.ParseInt(os.Getenv(key), 10, 64)
		if err != nil || id <= 0 {
			log.Fatalf("invalid %s", key)
		}
		if _, exists := routes[id]; exists {
			log.Fatal("installation identities must differ")
		}
		target, err := url.Parse(address)
		if err != nil {
			log.Fatal(err)
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = 30 * time.Second
		proxy.Transport = transport
		routes[id] = proxy
	}
	repositories := make(map[repositoryRoute]http.Handler)
	if os.Getenv("MANTL_RUNNERS_ENABLED") == "true" {
		id, err := strconv.ParseInt(os.Getenv("LEGACY_INSTALLATION_ID"), 10, 64)
		if err != nil || id <= 0 {
			log.Fatal("invalid Mantl installation identity")
		}
		target, err := url.Parse("http://127.0.0.1:8083")
		if err != nil {
			log.Fatal(err)
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = 30 * time.Second
		proxy.Transport = transport
		repositories[repositoryRoute{id, "somethingwithproof/mantl"}] = proxy
	}
	server := &http.Server{Addr: "127.0.0.1:8082", Handler: installationRouter(routes, repositories), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second}
	log.Fatal(server.ListenAndServe())
}
