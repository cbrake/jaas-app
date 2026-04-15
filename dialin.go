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

func (c *DialInClient) FetchPIN(appID, room string) (string, error) {
	conference := room + "@conference." + appID + ".8x8.vc"
	pinURL := c.baseURL() + "/v1/_jaas/vmms-conference-mapper/v1/access?conference=" + url.QueryEscape(conference)
	resp, err := http.Get(pinURL)
	if err != nil {
		return "", fmt.Errorf("fetching PIN: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("PIN API returned %d", resp.StatusCode)
	}

	var pinResp struct {
		ID json.Number `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pinResp); err != nil {
		return "", fmt.Errorf("decoding PIN response: %w", err)
	}

	pinInt, err := strconv.ParseInt(pinResp.ID.String(), 10, 64)
	if err != nil {
		return "", fmt.Errorf("parsing PIN: %w", err)
	}
	pinStr := fmt.Sprintf("%d", pinInt)
	if len(pinStr) > 4 {
		return pinStr[:len(pinStr)-4] + " " + pinStr[len(pinStr)-4:], nil
	}
	return pinStr, nil
}

func (c *DialInClient) FetchDIDs() (map[string][]string, error) {
	didsURL := c.baseURL() + "/v1/_jaas/vmms-conference-mapper/access/v1/dids"
	resp, err := http.Get(didsURL)
	if err != nil {
		return nil, fmt.Errorf("fetching DIDs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DIDs API returned %d", resp.StatusCode)
	}

	var dids []struct {
		CountryCode     string `json:"countryCode"`
		FormattedNumber string `json:"formattedNumber"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dids); err != nil {
		return nil, fmt.Errorf("decoding DIDs response: %w", err)
	}
	numbers := make(map[string][]string)
	for _, d := range dids {
		numbers[d.CountryCode] = append(numbers[d.CountryCode], d.FormattedNumber)
	}
	return numbers, nil
}

func (c *DialInClient) FetchDialInInfo(appID, room string) (*DialInInfo, error) {
	pin, err := c.FetchPIN(appID, room)
	if err != nil {
		return nil, err
	}
	numbers, err := c.FetchDIDs()
	if err != nil {
		return nil, err
	}
	return &DialInInfo{PIN: pin, Numbers: numbers}, nil
}
