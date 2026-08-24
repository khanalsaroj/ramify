// Command mockgithub is a minimal stand-in for the GitHub REST API, used only by
// the e2e harness: it records issue/PR comments so the test can assert
// providers/notify/githubcomment actually posted one.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"
)

type comment struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Body   string `json:"body"`
}

var (
	mu       sync.Mutex
	comments []comment
)

var commentPath = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues/(\d+)/comments$`)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleRoot)

	server := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(server.ListenAndServe())
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if m := commentPath.FindStringSubmatch(r.URL.Path); m != nil {
			handleCreateComment(w, r, m)
			return
		}
	}
	if r.Method == http.MethodGet && r.URL.Path == "/_test/comments" {
		handleListComments(w)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func handleCreateComment(w http.ResponseWriter, r *http.Request, m []string) {
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	number, _ := strconv.Atoi(m[3])

	mu.Lock()
	comments = append(comments, comment{Owner: m[1], Repo: m[2], Number: number, Body: body.Body})
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"id":1}`))
}

func handleListComments(w http.ResponseWriter) {
	mu.Lock()
	defer mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(comments)
}
