package models

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseAccountCatalog(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "slug is the identifier",
			body: `{"models":[{"slug":"gpt-5.6-sol"},{"slug":"gpt-5.5"}]}`,
			want: []string{"gpt-5.5", "gpt-5.6-sol"},
		},
		{
			// The manifest CPA publishes spells it "id". Accepting both means a
			// rename in either place degrades to a shorter list rather than to
			// no list at all.
			name: "id is accepted as a fallback",
			body: `{"models":[{"id":"gpt-5.5"}]}`,
			want: []string{"gpt-5.5"},
		},
		{
			name: "slug wins over id",
			body: `{"models":[{"slug":"real","id":"alias"}]}`,
			want: []string{"real"},
		},
		{
			name: "duplicates collapse",
			body: `{"models":[{"slug":"a"},{"slug":"a"},{"slug":"b"}]}`,
			want: []string{"a", "b"},
		},
		{
			name: "entries with neither field are skipped",
			body: `{"models":[{"display_name":"x"},{"slug":"a"},{"slug":"  "}]}`,
			want: []string{"a"},
		},
		{
			name: "no models key",
			body: `{}`,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAccountCatalog([]byte(tc.body))
			if err != nil {
				t.Fatalf("ParseAccountCatalog: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestParseAccountCatalog_RejectsMalformedJSON(t *testing.T) {
	if _, err := ParseAccountCatalog([]byte("not json")); err == nil {
		t.Fatal("want an error for a malformed body")
	}
}

// TestAccountFetcher_SendsTheAccountHeaders pins the request shape.
//
// The endpoint is not a published API: it answers the Codex client, and it
// decides what to return from the account header and the client version. A
// request without them is either rejected or answered with a different
// catalog generation, and both look like "this account has no models".
func TestAccountFetcher_SendsTheAccountHeaders(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotAccount, gotOriginator string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("client_version")
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("Chatgpt-Account-Id")
		gotOriginator = r.Header.Get("Originator")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{{"slug": "gpt-5.5"}},
		})
	}))
	defer srv.Close()

	fetcher := NewAccountFetcher(srv.URL)
	list, err := fetcher.List(t.Context(), "token-123", "acct-9")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if gotPath != ModelsPath {
		t.Errorf("path = %q, want %q", gotPath, ModelsPath)
	}
	if gotQuery == "" {
		t.Error("client_version was not sent")
	}
	if gotAuth != "Bearer token-123" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotAccount != "acct-9" {
		t.Errorf("Chatgpt-Account-Id = %q, want acct-9", gotAccount)
	}
	if gotOriginator != defaultOriginator {
		t.Errorf("Originator = %q, want %q", gotOriginator, defaultOriginator)
	}
	if len(list) != 1 || list[0] != "gpt-5.5" {
		t.Errorf("list = %v, want [gpt-5.5]", list)
	}
}

// TestAccountFetcher_OmitsTheAccountHeaderWhenUnknown: the header is optional
// upstream, and some credential layouts genuinely carry no account id. Sending
// an empty one is not the same as not sending it.
func TestAccountFetcher_OmitsTheAccountHeaderWhenUnknown(t *testing.T) {
	var present bool
	var value string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value = r.Header.Get("Chatgpt-Account-Id")
		_, present = r.Header["Chatgpt-Account-Id"]
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.5"}]}`))
	}))
	defer srv.Close()

	if _, err := NewAccountFetcher(srv.URL).List(t.Context(), "token", ""); err != nil {
		t.Fatalf("List: %v", err)
	}
	if present {
		t.Errorf("Chatgpt-Account-Id was sent as %q, want the header omitted", value)
	}
}

func TestAccountFetcher_ReportsUpstreamStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := NewAccountFetcher(srv.URL).List(t.Context(), "token", "")
	if err == nil {
		t.Fatal("want an error for a 401")
	}
}

// TestAccountFetcher_RejectsAnEmptyCatalog: an empty list would otherwise be
// stored and would silently replace a working one with nothing.
func TestAccountFetcher_RejectsAnEmptyCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	if _, err := NewAccountFetcher(srv.URL).List(t.Context(), "token", ""); err == nil {
		t.Fatal("want an error for an empty catalog")
	}
}

func TestAccountFetcher_RefusesAMissingToken(t *testing.T) {
	if _, err := NewAccountFetcher("").List(t.Context(), "", ""); err == nil {
		t.Fatal("want an error when there is no access token")
	}
}
