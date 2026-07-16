package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchPIN(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"id": 88342291,
		})
	}))
	defer server.Close()

	client := &DialInClient{BaseURL: server.URL}
	pin, err := client.FetchPIN("vpaas-test", "weekly-sync")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if pin != "8834 2291" {
		t.Errorf("PIN = %q, want %q", pin, "8834 2291")
	}
}

func TestFetchPINAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := &DialInClient{BaseURL: server.URL}
	_, err := client.FetchPIN("vpaas-test", "weekly-sync")
	if err == nil {
		t.Fatal("expected error on API failure")
	}
}

func TestFetchDIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]string{
			{"countryCode": "US", "formattedNumber": "+1-555-0123"},
			{"countryCode": "GB", "formattedNumber": "+44-20-7946-0958"},
		})
	}))
	defer server.Close()

	client := &DialInClient{BaseURL: server.URL}
	numbers, err := client.FetchDIDs()
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := numbers["US"]; len(got) != 1 || got[0] != "+1-555-0123" {
		t.Errorf("US numbers = %v, want [+1-555-0123]", got)
	}
	if got := numbers["GB"]; len(got) != 1 || got[0] != "+44-20-7946-0958" {
		t.Errorf("GB numbers = %v, want [+44-20-7946-0958]", got)
	}
}

func TestFetchDIDsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := &DialInClient{BaseURL: server.URL}
	_, err := client.FetchDIDs()
	if err == nil {
		t.Fatal("expected error on API failure")
	}
}
