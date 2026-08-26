// Command mockgithub is a minimal stand-in for the GitHub REST API, used only by
// the e2e harness: it records issue/PR comments so the test can assert
// providers/notify/githubcomment actually posted one.
//
// It implements the three calls UpsertPreviewComment makes, not just the create:
// listing is how that function finds an existing Ramify-owned comment to update,
// and editing is what it does when it finds one. A mock that only answered POST
// made every notify attempt fail on the list, which the harness reported as a
// missing comment rather than as the 404 it was.
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
	ID     int64  `json:"id"`
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Body   string `json:"body"`
}

var (
	mu       sync.Mutex
	comments []comment
	nextID   int64 = 1
)

var (
	commentsPath = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues/(\d+)/comments$`)
	editPath     = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues/comments/(\d+)$`)
)

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
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/_test/comments":
		handleTestListComments(w)
	case r.Method == http.MethodPost && commentsPath.MatchString(r.URL.Path):
		handleCreateComment(w, r, commentsPath.FindStringSubmatch(r.URL.Path))
	case r.Method == http.MethodGet && commentsPath.MatchString(r.URL.Path):
		handleListComments(w, commentsPath.FindStringSubmatch(r.URL.Path))
	case r.Method == http.MethodPatch && editPath.MatchString(r.URL.Path):
		handleEditComment(w, r, editPath.FindStringSubmatch(r.URL.Path))
	default:
		//nolint:gosec // G706: both values pass through sanitize, which strconv.Quote-escapes control characters; the taint analysis does not model the sanitizer
		log.Printf("mockgithub: unhandled %s %s", sanitize(r.Method), sanitize(r.URL.Path))
		w.WriteHeader(http.StatusNotFound)
	}
}

// sanitize renders a request-supplied value safe to log. Both the method and the
// path come from the caller, and a newline in either would let one request forge
// extra log lines. strconv.Quote escapes control characters, and the length cap
// keeps a long path from flooding the output.
func sanitize(v string) string {
	const maxLogged = 128
	if len(v) > maxLogged {
		v = v[:maxLogged] + "..."
	}
	return strconv.Quote(v)
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
	created := comment{ID: nextID, Owner: m[1], Repo: m[2], Number: number, Body: body.Body}
	nextID++
	comments = append(comments, created)
	mu.Unlock()

	writeJSON(w, http.StatusCreated, created)
}

// handleListComments returns the comments on one issue in the shape the GitHub
// client expects. Only id and body matter to UpsertPreviewComment, but the extra
// fields are harmless and keep the payload recognizable.
func handleListComments(w http.ResponseWriter, m []string) {
	number, _ := strconv.Atoi(m[3])

	mu.Lock()
	out := make([]comment, 0, len(comments))
	for _, c := range comments {
		if c.Owner == m[1] && c.Repo == m[2] && c.Number == number {
			out = append(out, c)
		}
	}
	mu.Unlock()

	writeJSON(w, http.StatusOK, out)
}

func handleEditComment(w http.ResponseWriter, r *http.Request, m []string) {
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	id, _ := strconv.ParseInt(m[3], 10, 64)

	mu.Lock()
	defer mu.Unlock()
	for i := range comments {
		if comments[i].ID == id {
			comments[i].Body = body.Body
			writeJSON(w, http.StatusOK, comments[i])
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

// handleTestListComments is the harness's own view: every comment, across
// issues, in insertion order.
func handleTestListComments(w http.ResponseWriter) {
	mu.Lock()
	defer mu.Unlock()
	writeJSON(w, http.StatusOK, comments)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
