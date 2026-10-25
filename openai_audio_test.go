package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenAIAudioSDKRequestAndMetadata(t *testing.T) {
	for _, languages := range [][]string{{"es"}, {"en"}, {"es", "en"}} {
		t.Run(strings.Join(languages, "-"), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/v1/audio/transcriptions" {
					t.Errorf("unexpected endpoint: %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer local-test-only" {
					t.Error("missing configured authorization")
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Errorf("multipart request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				defer r.MultipartForm.RemoveAll()
				if r.FormValue("model") != transcriptionModel || r.FormValue("response_format") != "json" {
					t.Errorf("model/format: %v", r.MultipartForm.Value)
				}
				for _, forbidden := range []string{"language", "temperature", "timestamp_granularities[]", "known_speaker_names[]", "known_speaker_references[]"} {
					if _, exists := r.MultipartForm.Value[forbidden]; exists {
						t.Errorf("legacy/diarization field was sent: %s", forbidden)
					}
				}
				if got := r.MultipartForm.Value["languages[]"]; !reflect.DeepEqual(got, languages) {
					t.Errorf("languages = %v; want %v; fields %v", got, languages, r.MultipartForm.Value)
				}
				if got := r.MultipartForm.Value["keywords[]"]; !reflect.DeepEqual(got, []string{"Odoo", "Transgenia"}) {
					t.Errorf("keywords = %v", got)
				}
				if r.FormValue("prompt") != "Odoo, Transgenia." {
					t.Error("short prompt not preserved")
				}
				file, header, err := r.FormFile("file")
				if err != nil {
					t.Errorf("file: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				defer file.Close()
				payload, _ := io.ReadAll(file)
				if !bytes.Equal(payload, []byte("local-audio-fixture")) || header.Filename != "window.webm" || header.Header.Get("Content-Type") != "audio/webm" {
					t.Error("file bytes or metadata changed")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("x-request-id", "req_mock_123")
				_, _ = io.WriteString(w, `{"text":"  Buenos días. Good morning.  ","languages":[{"code":"es"},{"code":"en"}],"usage":{"type":"tokens","input_tokens":7,"output_tokens":4,"total_tokens":11}}`)
			}))
			defer server.Close()
			tr := &openAITranscriber{BaseURL: server.URL + "/v1", Key: "local-test-only", Model: transcriptionModel, Prompt: "Odoo, Transgenia.", Keywords: []string{"Odoo", "Transgenia"}, HTTP: server.Client()}
			result, err := tr.Transcribe(withTranscriptionLanguages(context.Background(), languages), []byte("local-audio-fixture"), "nested/window.webm")
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || len(result) != 1 {
				t.Fatalf("calls=%d result=%+v", calls.Load(), result)
			}
			s := result[0]
			if s.Text != "Buenos días. Good morning." || s.TimePrecision != "window" || s.Start != 0 || s.End != 0 || s.AvgLogprob != 0 || s.NoSpeechProb != 0 {
				t.Fatalf("text or fabricated acoustic measurements: %+v", s)
			}
			if s.RequestID != "req_mock_123" || s.Model != transcriptionModel || !reflect.DeepEqual(s.Languages, languages) || !reflect.DeepEqual(s.DetectedLanguages, []string{"es", "en"}) {
				t.Fatalf("metadata: %+v", s)
			}
			var usage map[string]any
			if err := json.Unmarshal(s.Usage, &usage); err != nil || usage["total_tokens"] != float64(11) {
				t.Fatalf("usage: %s (%v)", s.Usage, err)
			}
		})
	}
}

func TestOpenAIAudioNoRetriesOrFallback(t *testing.T) {
	for _, status := range []int{400, 401, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("x-request-id", "req_failure")
				w.Header().Set("x-should-retry", "true")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"message":"sensitive customer words and local-test-only","type":"server_error","code":"failure"}}`)
			}))
			defer server.Close()
			tr := &openAITranscriber{BaseURL: server.URL, Key: "local-test-only", HTTP: server.Client()}
			_, err := tr.Transcribe(context.Background(), []byte("fixture"), "window.webm")
			var safe *audioTranscriptionError
			var httpErr *httpStatusError
			if !errors.As(err, &safe) || safe.StatusCode != status || safe.RequestID != "req_failure" || !errors.As(err, &httpErr) {
				t.Fatalf("safe status metadata missing: %v", err)
			}
			if strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "local-test-only") || strings.Contains(err.Error(), server.URL) {
				t.Fatalf("unsafe error: %s", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("SDK retried or fell back: %d requests", calls.Load())
			}
		})
	}
}

func TestOpenAIAudioRejectsMalformedAndEmpty(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"invalid json", "{broken", "unexpected response"},
		{"missing text", `{"segments":[{"start":3,"end":6,"text":"legacy"}]}`, "unexpected response"},
		{"null", "null", "unexpected response"},
		{"null text", `{"text":null}`, "unexpected response"},
		{"number text", `{"text":23}`, "unexpected response"},
		{"empty", `{"text":""}`, "empty transcript"},
		{"whitespace", `{"text":" \n\t "}`, "empty transcript"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("x-request-id", "req_parse")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			tr := &openAITranscriber{BaseURL: server.URL, Key: "local-test-only", HTTP: server.Client()}
			result, err := tr.Transcribe(context.Background(), []byte("fixture"), "window.webm")
			if err == nil || !strings.Contains(err.Error(), tc.want) || result != nil {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestOpenAIAudioTextDoesNotBecomeMeasuredSegments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-request-id", "invalid request id with spaces")
		_, _ = io.WriteString(w, `{"text":"One window.","segments":[{"start":3,"end":6,"text":"legacy"}],"usage":null}`)
	}))
	defer server.Close()
	tr := &openAITranscriber{BaseURL: server.URL, Key: "local-test-only", HTTP: server.Client()}
	result, err := tr.Transcribe(context.Background(), []byte("fixture"), "window.webm")
	if err != nil || len(result) != 1 {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if result[0].Start != 0 || result[0].End != 0 || result[0].TimePrecision != "window" || result[0].RequestID != "" || result[0].Usage != nil {
		t.Fatalf("unverified timing/metadata accepted: %+v", result[0])
	}
}

func TestOpenAIAudioEmptyStillPreservesBilledUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-request-id", "req_empty_billed")
		_, _ = io.WriteString(w, `{"text":"","usage":{"type":"duration","seconds":12.5}}`)
	}))
	defer server.Close()
	tr := &openAITranscriber{BaseURL: server.URL, Key: "local-test-only", HTTP: server.Client()}
	_, err := tr.Transcribe(context.Background(), []byte("fixture"), "window.webm")
	var safe *audioTranscriptionError
	if !errors.As(err, &safe) || safe.RequestID != "req_empty_billed" || !bytes.Contains(safe.Usage, []byte(`"seconds":12.5`)) {
		t.Fatalf("billed usage lost on empty output: %+v", err)
	}
}

func TestOpenAIAudioContextCancellation(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-finished:
		}
	}))
	defer server.Close()
	defer close(finished)
	tr := &openAITranscriber{BaseURL: server.URL, Key: "local-test-only", HTTP: server.Client()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := tr.Transcribe(ctx, []byte("fixture"), "window.webm")
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("mock request did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not stop request")
	}
}

func TestOpenAIAudioTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	tr := &openAITranscriber{BaseURL: server.URL, Key: "local-test-only", HTTP: server.Client(), Timeout: 25 * time.Millisecond}
	_, err := tr.Transcribe(context.Background(), []byte("fixture"), "window.webm")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout lost: %v", err)
	}
}

func TestOpenAIAudioInputValidationDoesNotCallProvider(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Error("invalid input reached provider")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	cases := []struct {
		name     string
		change   func(*openAITranscriber)
		audio    []byte
		filename string
	}{
		{"legacy model", func(tr *openAITranscriber) { tr.Model = "whisper-1" }, []byte("fixture"), "window.webm"},
		{"missing key", func(tr *openAITranscriber) { tr.Key = "" }, []byte("fixture"), "window.webm"},
		{"empty audio", func(tr *openAITranscriber) {}, nil, "window.webm"},
		{"unsupported filename", func(tr *openAITranscriber) {}, []byte("fixture"), "window.exe"},
		{"invalid filename", func(tr *openAITranscriber) {}, []byte("fixture"), "x\n.webm"},
		{"invalid language", func(tr *openAITranscriber) { tr.Languages = []string{"mixed"} }, []byte("fixture"), "window.webm"},
		{"long prompt", func(tr *openAITranscriber) { tr.Prompt = strings.Repeat("a", 513) }, []byte("fixture"), "window.webm"},
		{"long keyword", func(tr *openAITranscriber) { tr.Keywords = []string{strings.Repeat("a", 65)} }, []byte("fixture"), "window.webm"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &openAITranscriber{BaseURL: server.URL, Key: "local-test-only", HTTP: server.Client()}
			tc.change(tr)
			if _, err := tr.Transcribe(context.Background(), tc.audio, tc.filename); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("provider called %d times", calls.Load())
	}
}

func TestTranscriptionLanguagesAreCopiedAndContextWins(t *testing.T) {
	languages := []string{"en", "es", "en"}
	ctx := withTranscriptionLanguages(context.Background(), languages)
	languages[0] = "invalid"
	tr := &openAITranscriber{Languages: []string{"es"}}
	got, err := tr.languages(ctx)
	if err != nil || !reflect.DeepEqual(got, []string{"en", "es"}) {
		t.Fatalf("context language selection = %v, %v", got, err)
	}
	got, err = tr.languages(context.Background())
	if err != nil || !reflect.DeepEqual(got, []string{"es"}) {
		t.Fatalf("configured language selection = %v, %v", got, err)
	}
}
