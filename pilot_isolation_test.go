package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestMeetingASRIsolation(t *testing.T) {
	tests := []struct {
		name              string
		roomTranscription bool
		transcriber       bool
		model             string
		wantCapture       bool
		wantJaaS          bool
	}{
		{"browser_gpt", true, true, transcriptionModel, true, false},
		{"browser_without_model_hint", true, true, "", true, false},
		{"gpt_missing_transcriber", true, false, transcriptionModel, false, false},
		{"room_disabled", false, true, transcriptionModel, false, false},
		{"legacy_jaas_only", true, false, "", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, transcriber := captureEnv(t)
			e.app.Config.TranscribeModel = tc.model
			if !tc.transcriber {
				e.app.Transcriber = nil
			}
			if err := e.app.DB.SetTranscription("sala-captura", tc.roomTranscription); err != nil {
				t.Fatal(err)
			}
			for _, moderator := range []bool{false, true} {
				t.Run("moderator_"+strconv.FormatBool(moderator), func(t *testing.T) {
					joined := httptest.NewRecorder()
					if err := e.app.startParticipant(joined, "sala-captura", "Pilot participant", "pilot@example.test", moderator); err != nil {
						t.Fatal(err)
					}
					browser := e.browser()
					for _, cookie := range joined.Result().Cookies() {
						browser.cookies[cookie.Name] = cookie
					}
					page := browser.do(http.MethodGet, "/m/sala-captura", nil)
					if page.Code != http.StatusOK {
						t.Fatalf("meeting status = %d", page.Code)
					}
					if !strings.Contains(page.Body.String(), `data-capture="`+strconv.FormatBool(tc.wantCapture)+`"`) {
						t.Fatalf("browser capture does not match %t", tc.wantCapture)
					}
					claims := jwtClaims(t, e, page.Body.String())
					features := claims["context"].(map[string]any)["features"].(map[string]any)
					if got, ok := features["transcription"].(bool); !ok || got != tc.wantJaaS {
						t.Fatalf("signed JaaS transcription = %v, want %t", features["transcription"], tc.wantJaaS)
					}
					if isModerator(claims) != moderator || features["recording"] != moderator {
						t.Fatal("moderator or recording permission changed")
					}
					room, err := e.app.DB.GetRoom("sala-captura")
					if err != nil || room.Transcription != tc.roomTranscription {
						t.Fatal("rendering changed the room transcription setting")
					}
				})
			}
			transcriber.mu.Lock()
			calls := transcriber.calls
			transcriber.mu.Unlock()
			if calls != 0 {
				t.Fatalf("rendering dispatched %d ASR requests", calls)
			}
		})
	}
}

func TestVisualizationLoadsApplicationScriptOnce(t *testing.T) {
	page, err := visualizationFS.ReadFile("visualization/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(page), `src="/visualizations/assets/app.js"`); got != 1 {
		t.Fatalf("visualization app.js loads = %d, want 1", got)
	}
}
