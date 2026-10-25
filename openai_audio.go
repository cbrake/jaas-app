package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

const (
	transcriptionModel          = "gpt-transcribe"
	defaultTranscriptionTimeout = 2 * time.Minute
	maxTranscriptionAudioBytes  = 25_000_000
)

// Start and End are only acoustic timestamps when TimePrecision explicitly says
// so. JSON text from gpt-transcribe has no segment timestamps: zero is the
// contextual start of the submitted window, not a measured speech boundary.
type transcriptSegment struct {
	Start             float64         `json:"start"`
	End               float64         `json:"end"`
	Text              string          `json:"text"`
	AvgLogprob        float64         `json:"avg_logprob,omitempty"`
	NoSpeechProb      float64         `json:"no_speech_prob,omitempty"`
	TimePrecision     string          `json:"time_precision,omitempty"`
	RequestID         string          `json:"request_id,omitempty"`
	Model             string          `json:"model,omitempty"`
	Languages         []string        `json:"languages,omitempty"`
	DetectedLanguages []string        `json:"detected_languages,omitempty"`
	Usage             json.RawMessage `json:"usage,omitempty"`
}

type Transcriber interface {
	Transcribe(context.Context, []byte, string) ([]transcriptSegment, error)
}

type openAITranscriber struct {
	BaseURL   string
	Key       string
	Model     string
	Prompt    string
	HTTP      *http.Client
	Languages []string
	Keywords  []string
	Timeout   time.Duration
}

type transcriptionLanguagesKey struct{}

// Copy the slice so the caller cannot change a running request's language mode.
func withTranscriptionLanguages(ctx context.Context, languages []string) context.Context {
	return context.WithValue(ctx, transcriptionLanguagesKey{}, append([]string(nil), languages...))
}

func (t *openAITranscriber) languages(ctx context.Context) ([]string, error) {
	languages := t.Languages
	if selected, ok := ctx.Value(transcriptionLanguagesKey{}).([]string); ok {
		languages = selected
	}
	if len(languages) == 0 {
		languages = []string{"es"}
	}
	result := make([]string, 0, 2)
	seen := make(map[string]bool)
	for _, language := range languages {
		if language != "es" && language != "en" {
			return nil, errors.New("openai audio: unsupported language mode")
		}
		if !seen[language] {
			result = append(result, language)
			seen[language] = true
		}
	}
	return result, nil
}

// Never retain the SDK error: its response/request dump may contain customer
// text or authorization data. Only safe status and request identity survive.
type audioTranscriptionError struct {
	Kind       string
	RequestID  string
	StatusCode int
	Usage      json.RawMessage
	cause      error
}

func (e *audioTranscriptionError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("openai audio: HTTP %d", e.StatusCode)
	}
	return "openai audio: " + e.Kind
}

func (e *audioTranscriptionError) Unwrap() error { return e.cause }

func (e *audioTranscriptionError) Permanent() bool {
	if e.StatusCode != 0 {
		return e.StatusCode >= 400 && e.StatusCode < 500 && e.StatusCode != http.StatusTooManyRequests
	}
	return e.Kind != "connection failed" && e.Kind != "request canceled" && e.Kind != "request timed out"
}

func safeAudioRequestID(response *http.Response) string {
	if response == nil {
		return ""
	}
	value := response.Header.Get("x-request-id")
	if len(value) == 0 || len(value) > 128 {
		return ""
	}
	for _, ch := range value {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_') {
			return ""
		}
	}
	return value
}

func audioFileMetadata(filename string) (string, string, error) {
	filename = path.Base(strings.ReplaceAll(filename, "\\", "/"))
	if filename == "." || filename == "/" || strings.ContainsAny(filename, "\r\n\x00") {
		return "", "", errors.New("openai audio: invalid filename")
	}
	contentTypes := map[string]string{
		".webm": "audio/webm", ".wav": "audio/wav", ".flac": "audio/flac",
		".mp3": "audio/mpeg", ".mpga": "audio/mpeg", ".mpeg": "audio/mpeg",
		".m4a": "audio/mp4", ".mp4": "video/mp4", ".ogg": "audio/ogg",
	}
	contentType, ok := contentTypes[strings.ToLower(path.Ext(filename))]
	if !ok {
		return "", "", errors.New("openai audio: unsupported audio format")
	}
	return filename, contentType, nil
}

func (t *openAITranscriber) Transcribe(ctx context.Context, audio []byte, filename string) ([]transcriptSegment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t.Model != "" && t.Model != transcriptionModel {
		return nil, errors.New("openai audio: only gpt-transcribe is enabled")
	}
	if strings.TrimSpace(t.Key) == "" {
		return nil, errors.New("openai audio: missing credentials")
	}
	if len(audio) == 0 || len(audio) >= maxTranscriptionAudioBytes {
		return nil, errors.New("openai audio: audio must be nonempty and smaller than 25 MB")
	}
	filename, contentType, err := audioFileMetadata(filename)
	if err != nil {
		return nil, err
	}
	languages, err := t.languages(ctx)
	if err != nil {
		return nil, err
	}
	prompt := strings.TrimSpace(t.Prompt)
	if utf8.RuneCountInString(prompt) > 512 || len(t.Keywords) > 32 {
		return nil, errors.New("openai audio: transcription guidance is too long")
	}
	keywords := make([]string, 0, len(t.Keywords))
	keywordChars := 0
	for _, keyword := range t.Keywords {
		keyword = strings.TrimSpace(keyword)
		if keyword == "" {
			continue
		}
		size := utf8.RuneCountInString(keyword)
		keywordChars += size
		if size > 64 || keywordChars > 512 {
			return nil, errors.New("openai audio: transcription guidance is too long")
		}
		keywords = append(keywords, keyword)
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = defaultTranscriptionTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	baseURL := t.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	options := []option.RequestOption{
		option.WithAPIKey(t.Key),
		option.WithBaseURL(baseURL),
		option.WithMaxRetries(0),
	}
	if t.HTTP != nil {
		options = append(options, option.WithHTTPClient(t.HTTP))
	}
	client := openai.NewClient(options...)
	params := openai.AudioTranscriptionNewParams{
		File:           openai.File(bytes.NewReader(audio), filename, contentType),
		Model:          openai.AudioModel(transcriptionModel),
		Languages:      languages,
		Keywords:       keywords,
		ResponseFormat: openai.AudioResponseFormatJSON,
	}
	if prompt != "" {
		params.Prompt = openai.String(prompt)
	}
	var response *http.Response
	out, err := client.Audio.Transcriptions.New(requestCtx, params, option.WithResponseInto(&response))
	requestID := safeAudioRequestID(response)
	if err != nil {
		if contextErr := requestCtx.Err(); contextErr != nil {
			kind := "request canceled"
			if errors.Is(contextErr, context.DeadlineExceeded) {
				kind = "request timed out"
			}
			return nil, &audioTranscriptionError{Kind: kind, RequestID: requestID, cause: contextErr}
		}
		if response != nil && response.StatusCode >= 400 {
			status := &httpStatusError{Op: "openai audio", Code: response.StatusCode}
			return nil, &audioTranscriptionError{StatusCode: response.StatusCode, RequestID: requestID, cause: status}
		}
		kind := "connection failed"
		if response != nil {
			kind = "unexpected response"
		}
		return nil, &audioTranscriptionError{Kind: kind, RequestID: requestID}
	}
	if out == nil {
		return nil, &audioTranscriptionError{Kind: "unexpected response", RequestID: requestID}
	}
	// Validate the required JSON text field independently of the SDK's permissive
	// response unions. Do not accept Whisper segments or fabricate their times.
	var raw struct {
		Text  *string         `json:"text"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(out.RawJSON()), &raw); err != nil || raw.Text == nil {
		return nil, &audioTranscriptionError{Kind: "unexpected response", RequestID: requestID}
	}
	usage := bytes.TrimSpace(raw.Usage)
	if len(usage) == 0 || bytes.Equal(usage, []byte("null")) || usage[0] != '{' {
		usage = nil
	}
	text := strings.TrimSpace(*raw.Text)
	if text == "" {
		return nil, &audioTranscriptionError{Kind: "empty transcript", RequestID: requestID, Usage: append(json.RawMessage(nil), usage...)}
	}
	detected := make([]string, 0, len(out.Languages))
	for _, language := range out.Languages {
		if language.Code != "" {
			detected = append(detected, language.Code)
		}
	}
	return []transcriptSegment{{
		Text: text, TimePrecision: "window", RequestID: requestID,
		Model: transcriptionModel, Languages: languages, DetectedLanguages: detected,
		Usage: append(json.RawMessage(nil), usage...),
	}}, nil
}
