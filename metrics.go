package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Conversation metrics are measured, not generated: they come from the
// timestamps of each utterance (whisper segments for browser capture). They
// are the "audio" side of the sentiment analysis we can afford without an
// acoustic model: who talks, how much, how fast and who interrupts whom.
// They are also passed to the model as context (paralinguistic metadata).

type SpeakerMetrics struct {
	Speaker        string  `json:"speaker"`
	Turns          int     `json:"turns"`
	Words          int     `json:"words"`
	TalkSeconds    float64 `json:"talk_seconds"`
	SharePct       float64 `json:"share_pct"`
	WordsPerMinute float64 `json:"words_per_minute"`
	LongestTurnSec float64 `json:"longest_turn_seconds"`
	Interruptions  int     `json:"interruptions"`
}

type ConversationMetrics struct {
	DurationSeconds float64          `json:"duration_seconds"`
	Measured        bool             `json:"measured"` // false when durations had to be estimated
	Speakers        []SpeakerMetrics `json:"speakers"`
}

type utterance struct {
	Name     string
	Text     string
	Start    time.Time
	Duration time.Duration
}

// parseUtterances reads the transcript JSON (JaaS shape plus our optional
// duration_ms) into utterances ordered by time.
func parseUtterances(data string) []utterance {
	var raw struct {
		Messages []struct {
			Name       string `json:"name"`
			Content    string `json:"content"`
			Text       string `json:"text"`
			Timestamp  int64  `json:"timestamp"`
			DurationMs int64  `json:"duration_ms"`
		} `json:"messages"`
	}
	if json.Unmarshal([]byte(data), &raw) != nil {
		return nil
	}
	var out []utterance
	for _, m := range raw.Messages {
		text := strings.TrimSpace(m.Content)
		if text == "" {
			text = strings.TrimSpace(m.Text)
		}
		if text == "" || m.Timestamp <= 0 {
			continue
		}
		name := strings.TrimSpace(m.Name)
		if name == "" {
			name = "Participante"
		}
		out = append(out, utterance{Name: name, Text: text, Start: time.UnixMilli(m.Timestamp), Duration: time.Duration(m.DurationMs) * time.Millisecond})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

const estimatedWordsPerSecond = 2.3 // ~140 wpm, Spanish conversational pace

func round1(f float64) float64 { return math.Round(f*10) / 10 }

// computeMetrics aggregates per-speaker participation. An utterance that
// starts before the previous speaker's utterance ends counts as an
// interruption by the newcomer.
func computeMetrics(us []utterance) *ConversationMetrics {
	if len(us) == 0 {
		return nil
	}
	measured := true
	byName := map[string]*SpeakerMetrics{}
	var order []string
	var total float64
	var lastEnd time.Time
	lastSpeaker := ""
	for i, u := range us {
		words := len(strings.Fields(u.Text))
		dur := u.Duration.Seconds()
		if dur <= 0 {
			measured = false
			dur = float64(words) / estimatedWordsPerSecond
		}
		s, ok := byName[u.Name]
		if !ok {
			s = &SpeakerMetrics{Speaker: u.Name}
			byName[u.Name] = s
			order = append(order, u.Name)
		}
		if u.Name != lastSpeaker {
			s.Turns++
			if i > 0 && u.Start.Before(lastEnd.Add(-300*time.Millisecond)) {
				s.Interruptions++
			}
		}
		s.Words += words
		s.TalkSeconds += dur
		if dur > s.LongestTurnSec {
			s.LongestTurnSec = dur
		}
		total += dur
		end := u.Start.Add(time.Duration(dur * float64(time.Second)))
		if end.After(lastEnd) {
			lastEnd = end
		}
		lastSpeaker = u.Name
	}
	m := &ConversationMetrics{Measured: measured, DurationSeconds: round1(lastEnd.Sub(us[0].Start).Seconds())}
	for _, name := range order {
		s := byName[name]
		if total > 0 {
			s.SharePct = round1(100 * s.TalkSeconds / total)
		}
		if s.TalkSeconds > 0 {
			s.WordsPerMinute = math.Round(float64(s.Words) / (s.TalkSeconds / 60))
		}
		s.TalkSeconds = round1(s.TalkSeconds)
		s.LongestTurnSec = round1(s.LongestTurnSec)
		m.Speakers = append(m.Speakers, *s)
	}
	sort.SliceStable(m.Speakers, func(i, j int) bool { return m.Speakers[i].TalkSeconds > m.Speakers[j].TalkSeconds })
	return m
}

// metricsForPrompt renders the measured participation for the model.
func metricsForPrompt(m *ConversationMetrics) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	src := "medidos del audio"
	if !m.Measured {
		src = "estimados por cantidad de palabras"
	}
	fmt.Fprintf(&b, "Datos de participación (%s; duración %s):\n", src, fmtDuration(m.DurationSeconds))
	for _, s := range m.Speakers {
		fmt.Fprintf(&b, "- %s: %.0f%% del tiempo de habla, %d turnos, %.0f palabras/min, turno más largo %s, interrumpió %d veces\n",
			s.Speaker, s.SharePct, s.Turns, s.WordsPerMinute, fmtDuration(s.LongestTurnSec), s.Interruptions)
	}
	return b.String()
}

func fmtDuration(sec float64) string {
	d := time.Duration(sec * float64(time.Second)).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%d s", int(d.Seconds()))
	}
	return fmt.Sprintf("%d min %02d s", int(d.Minutes()), int(d.Seconds())%60)
}
