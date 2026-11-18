package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// HTTP server and trial lifecycle. The forge itself lives in the other files;
// this file only routes requests and owns the env register/done protocol.
// ---------------------------------------------------------------------------

var requestCounter int64

// Lock helpers whose Unlock runs even when the wrapped handler panics, so a
// single bad request can never wedge the whole forge.
func withLock(fn func() Reply) (reply Reply) {
	stateMu.Lock()
	defer stateMu.Unlock()
	return fn()
}

func withRLock(fn func() Reply) (reply Reply) {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return fn()
}

func withRLockVal(fn func() *Repo) (repo *Repo) {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return fn()
}

func expectedToken() string {
	return os.Getenv("MAKEFUN_SHOP_TOKEN")
}

func isAuthorized(r *http.Request) bool {
	token := expectedToken()
	if token == "" {
		return true
	}
	return r.Header.Get("Authorization") == "Bearer "+token
}

func sendReply(w http.ResponseWriter, reply Reply) {
	n := atomic.AddInt64(&requestCounter, 1)
	header := w.Header()
	header.Set("Content-Type", reply.ContentType)
	header.Set("X-GitHub-Request-Id", fmt.Sprintf("req-%d", n))
	for k, v := range reply.Headers {
		header.Set(k, v)
	}
	w.WriteHeader(reply.Status)
	if len(reply.Body) > 0 {
		_, _ = w.Write(reply.Body)
	}
}

func splitPath(path string) []string {
	var segments []string
	for _, part := range strings.Split(path, "/") {
		if part == "" {
			continue
		}
		if decoded, err := url.PathUnescape(part); err == nil {
			segments = append(segments, decoded)
		} else {
			segments = append(segments, part)
		}
	}
	return segments
}

func readRequestBody(r *http.Request) []byte {
	if r.Method != "POST" && r.Method != "PUT" && r.Method != "PATCH" && r.Method != "DELETE" {
		return nil
	}
	if r.Body == nil {
		return nil
	}
	body, _ := io.ReadAll(r.Body)
	return body
}

func dispatch(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "panic handling %s %s: %v\n", r.Method, r.URL.Path, rec)
			sendReply(w, jsonReply(500, map[string]interface{}{
				"message": fmt.Sprintf("Internal error: %v", rec),
			}, nil))
		}
	}()
	parsed, _ := url.Parse(r.URL.RequestURI())
	path := parsed.Path
	query := parsed.Query()
	segments := splitPath(path)
	host := r.Host
	if host == "" {
		host = "127.0.0.1"
	}
	body := readRequestBody(r)

	if path == "/healthz" {
		sendReply(w, jsonReply(200, map[string]interface{}{"status": "ok"}, nil))
		return
	}
	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
		if !isAuthorized(r) {
			sendReply(w, jsonReply(401, map[string]interface{}{"message": "Bad credentials"}, nil))
			return
		}
		if r.Method == "GET" {
			sendReply(w, withRLock(func() Reply {
				return restReply(r.Method, segments[2:], query, body, host, path)
			}))
		} else {
			sendReply(w, withLock(func() Reply {
				return restReply(r.Method, segments[2:], query, body, host, path)
			}))
		}
		return
	}
	if len(segments) == 2 && segments[0] == "api" && segments[1] == "graphql" {
		if r.Method != "POST" {
			sendReply(w, notFound())
			return
		}
		if !isAuthorized(r) {
			sendReply(w, jsonReply(401, map[string]interface{}{"message": "Bad credentials"}, nil))
			return
		}
		sendReply(w, withLock(func() Reply { return graphqlReply(body) }))
		return
	}
	if len(segments) == 1 && segments[0] == "mcp" {
		if r.Method != "POST" {
			sendReply(w, notFound())
			return
		}
		if !isAuthorized(r) {
			sendReply(w, jsonReply(401, map[string]interface{}{
				"jsonrpc": "2.0", "id": nil,
				"error": map[string]interface{}{"code": -32001, "message": "Unauthorized"},
			}, nil))
			return
		}
		sendReply(w, withLock(func() Reply { return mcpReply(body) }))
		return
	}
	if len(segments) >= 3 && strings.HasSuffix(segments[1], ".git") {
		dispatchGit(w, r, segments, query, body)
		return
	}
	sendReply(w, notFound())
}

func dispatchGit(w http.ResponseWriter, r *http.Request, segments []string, query url.Values, body []byte) {
	owner := segments[0]
	repoName := strings.TrimSuffix(segments[1], ".git")
	tail := segments[2:]
	if !isAuthorized(r) {
		sendReply(w, jsonReply(401, map[string]interface{}{"message": "Bad credentials"}, nil))
		return
	}
	repo := withRLockVal(func() *Repo { return FORGE.GetRepo(owner, repoName) })
	if repo == nil {
		sendReply(w, notFound())
		return
	}
	version2 := strings.Contains(r.Header.Get("Git-Protocol"), "version=2")
	isInfoRefs := len(tail) == 2 && tail[0] == "info" && tail[1] == "refs" && r.Method == "GET"
	isUploadPack := len(tail) == 1 && tail[0] == "git-upload-pack" && r.Method == "POST"
	isReceivePack := len(tail) == 1 && tail[0] == "git-receive-pack" && r.Method == "POST"
	switch {
	case isInfoRefs:
		service := query.Get("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			n := atomic.AddInt64(&requestCounter, 1)
			header := w.Header()
			header.Set("Content-Type", "text/plain")
			header.Set("X-GitHub-Request-Id", fmt.Sprintf("req-%d", n))
			w.WriteHeader(400)
			_, _ = w.Write([]byte("unsupported service"))
			return
		}
		sendReply(w, withRLock(func() Reply { return infoRefsReply(repo, service, version2) }))
	case isUploadPack:
		sendReply(w, withRLock(func() Reply { return uploadPackReply(repo, body, version2) }))
	case isReceivePack:
		sendReply(w, withLock(func() Reply { return receivePackReply(repo, body) }))
	default:
		sendReply(w, notFound())
	}
}

func envPostJSON(url string, payload map[string]interface{}) (int, error) {
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(payload)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(url, "application/json", &buf)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func envGetJSON(url string) (map[string]interface{}, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	server := &http.Server{Handler: http.HandlerFunc(dispatch)}
	if envURL == "" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "0"
		}
		listener, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "forge listening on 127.0.0.1:%d\n", listener.Addr().(*net.TCPAddr).Port)
		_ = server.Serve(listener)
		return
	}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	go func() { _ = server.Serve(listener) }()
	registered := false
	for i := 0; i < 120; i++ {
		status, err := envPostJSON(envURL+"/register", map[string]interface{}{"port": port})
		if err == nil && (status == 200 || status == 409) {
			registered = true
			break
		}
		if err == nil && status == 409 {
			registered = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !registered {
		fmt.Fprintln(os.Stderr, "warning: could not register port with env")
	}
	delay := 200 * time.Millisecond
	for {
		state, err := envGetJSON(envURL + "/done")
		if err == nil {
			if done, _ := state["done"].(bool); done {
				break
			}
			delay = 200 * time.Millisecond
		} else {
			delay = time.Duration(float64(delay) * 1.5)
			if delay > 2*time.Second {
				delay = 2 * time.Second
			}
		}
		time.Sleep(delay)
	}
	_ = server.Close()
}
