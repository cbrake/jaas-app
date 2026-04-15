package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchDialInInfo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/_jaas/vmms-conference-mapper/access/v1/dids", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]string{
			{"countryCode": "US", "formattedNumber": "+1-555-0123"},
			{"countryCode": "GB", "formattedNumber": "+44-20-7946-0958"},
		})
	})
	mux.HandleFunc("/v1/_jaas/vmms-conference-mapper/v1/access", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"id": 88342291,
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := &DialInClient{BaseURL: server.URL}
	info, err := client.FetchDialInInfo("vpaas-test", "weekly-sync")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if info.PIN == "" {
		t.Error("PIN should not be empty")
	}
	if len(info.Numbers) == 0 {
		t.Error("should have at least one number")
	}
}

func TestFetchDialInInfoAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := &DialInClient{BaseURL: server.URL}
	_, err := client.FetchDialInInfo("vpaas-test", "weekly-sync")
	if err == nil {
		t.Fatal("expected error on API failure")
	}
}
