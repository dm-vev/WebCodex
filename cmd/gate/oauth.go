package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

func (s *server) handleProtectedResource(w http.ResponseWriter, r *http.Request) {
	log.Printf("oauth protected-resource %s %s ua=%q", r.Method, r.URL.Path, r.UserAgent())
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]any{
		"resource":              s.publicURL + "/mcp",
		"authorization_servers": []string{s.publicURL},
	})
}

func (s *server) handleOAuthServer(w http.ResponseWriter, r *http.Request) {
	log.Printf("oauth server-metadata %s %s ua=%q", r.Method, r.URL.Path, r.UserAgent())
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]any{
		"issuer":                                s.publicURL,
		"authorization_endpoint":                s.publicURL + "/oauth/authorize",
		"token_endpoint":                        s.publicURL + "/oauth/token",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"code_challenge_methods_supported":      []string{"S256", "plain"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic"},
		"scopes_supported":                      []string{"mcp"},
	})
}

// handleAuthorize redirects the configured client with the fixed code expected by this single-client gate.
func (s *server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	log.Printf(
		"oauth authorize client_id=%q redirect_uri=%q ua=%q",
		r.URL.Query().Get("client_id"),
		r.URL.Query().Get("redirect_uri"),
		r.UserAgent(),
	)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.oauthID != "" && r.URL.Query().Get("client_id") != s.oauthID {
		http.Error(w, "unknown client", http.StatusUnauthorized)
		return
	}
	redirectURI := r.URL.Query().Get("redirect_uri")
	if redirectURI == "" {
		http.Error(w, "missing redirect_uri", http.StatusBadRequest)
		return
	}
	target, err := http.NewRequest(http.MethodGet, redirectURI, nil)
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	query := target.URL.Query()
	query.Set("code", "webcodex")
	if state := r.URL.Query().Get("state"); state != "" {
		query.Set("state", state)
	}
	target.URL.RawQuery = query.Encode()
	http.Redirect(w, r, target.URL.String(), http.StatusFound)
}

func (s *server) handleToken(w http.ResponseWriter, r *http.Request) {
	log.Printf("oauth token %s content_type=%q ua=%q", r.Method, r.Header.Get("Content-Type"), r.UserAgent())
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	params, err := tokenParams(r)
	if err != nil {
		http.Error(w, "bad token request", http.StatusBadRequest)
		return
	}
	clientID, clientSecret := params["client_id"], params["client_secret"]
	if authID, authSecret, ok := r.BasicAuth(); ok {
		clientID, clientSecret = authID, authSecret
	}
	if s.oauthID != "" && clientID != s.oauthID {
		http.Error(w, "unknown client", http.StatusUnauthorized)
		return
	}
	if s.oauthSecret != "" && clientSecret != s.oauthSecret {
		http.Error(w, "bad client secret", http.StatusUnauthorized)
		return
	}
	if params["grant_type"] != "authorization_code" {
		http.Error(w, "unsupported grant_type", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{
		"access_token": s.publicToken,
		"token_type":   "Bearer",
		"expires_in":   31536000,
		"scope":        "mcp",
	})
}

func tokenParams(r *http.Request) (map[string]string, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var values map[string]string
		if err := json.NewDecoder(r.Body).Decode(&values); err != nil {
			return nil, fmt.Errorf("decode token request: %w", err)
		}
		return values, nil
	}
	if err := r.ParseForm(); err != nil {
		return nil, fmt.Errorf("parse token form: %w", err)
	}
	values := make(map[string]string, len(r.Form))
	for key := range r.Form {
		values[key] = r.Form.Get(key)
	}
	return values, nil
}
