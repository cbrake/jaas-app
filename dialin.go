package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

const defaultBaseURL = "https://8x8.vc"

type DialInInfo struct {
	PIN     string
	Numbers map[string][]string // country code -> phone numbers
}

type DialInClient struct {
	BaseURL string
}

func (c *DialInClient) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return defaultBaseURL
}

func (c *DialInClient) FetchDialInInfo(appID, room string) (*DialInInfo, error) {
	info := &DialInInfo{}

	// Fetch PIN
	conference := room + "@conference." + appID + ".8x8.vc"
	pinURL := c.baseURL() + "/v1/_jaas/vmms-conference-mapper/v1/access?conference=" + url.QueryEscape(conference)
	resp, err := http.Get(pinURL)
	if err != nil {
		return nil, fmt.Errorf("fetching PIN: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("PIN API returned %d", resp.StatusCode)
	}

	var pinResp struct {
		ID json.Number `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pinResp); err != nil {
		return nil, fmt.Errorf("decoding PIN response: %w", err)
	}

	pinInt, err := strconv.ParseInt(pinResp.ID.String(), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parsing PIN: %w", err)
	}
	// Format as groups of 4 digits for readability
	pinStr := fmt.Sprintf("%d", pinInt)
	if len(pinStr) > 4 {
		info.PIN = pinStr[:len(pinStr)-4] + " " + pinStr[len(pinStr)-4:]
	} else {
		info.PIN = pinStr
	}

	// Fetch DIDs
	didsURL := c.baseURL() + "/v1/_jaas/vmms-conference-mapper/access/v1/dids"
	resp2, err := http.Get(didsURL)
	if err != nil {
		return nil, fmt.Errorf("fetching DIDs: %w", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DIDs API returned %d", resp2.StatusCode)
	}

	var didsResp struct {
		Numbers map[string][]string `json:"numbers"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&didsResp); err != nil {
		return nil, fmt.Errorf("decoding DIDs response: %w", err)
	}
	info.Numbers = didsResp.Numbers

	return info, nil
}
