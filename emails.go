package main

import (
	"bytes"
	"html/template"
	"strings"
	"time"
)

// HTML e-mails use the conservative e-mail subset: 600 px nested tables,
// inline CSS only, no remote images or web fonts, and a plain-text
// alternative. Every text/background pair passes WCAG AA contrast.

const (
	cInk      = "#111827"
	cMuted    = "#4B5563"
	cHeader   = "#1F2937"
	cPositive = "#166534"
	cNegative = "#B91C1C"
	cOnColor  = "#FFFFFF"
)

var emailFuncs = template.FuncMap{
	"pill": func(v string) template.CSS {
		bg := cHeader
		switch v {
		case "positive":
			bg = cPositive
		case "neutral":
			bg = cMuted
		case "negative":
			bg = cNegative
		}
		return template.CSS("display:inline-block;background-color:" + bg + ";color:" + cOnColor +
			";font-family:Arial,Helvetica,sans-serif;font-size:11px;font-weight:bold;letter-spacing:1px;text-transform:uppercase;padding:3px 9px;border-radius:10px;")
	},
	"pct": func(f float64) int {
		if f < 1 {
			return 1
		}
		if f > 99 {
			return 99
		}
		return int(f + 0.5)
	},
	"sub": func(a, b int) int { return a - b },
	"dur": fmtDuration,
	"deref": func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	},
	"phase": func(p string) string {
		switch p {
		case "beginning":
			return "Beginning"
		case "middle":
			return "Middle"
		}
		return "End"
	},
	"odd": func(i int) bool { return i%2 == 1 },
}

// Shared frame. Blocks: preheader, header, accent rule, body, footer.
const emailFrame = `{{define "head"}}<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Subject}}</title></head>
<body style="margin:0;padding:0;background-color:#F3F4F6;">
<div style="display:none;max-height:0;overflow:hidden;opacity:0;">{{.Preheader}}</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#F3F4F6;">
<tr><td align="center" style="padding:24px 12px;">
<table role="presentation" width="600" cellpadding="0" cellspacing="0" border="0" style="width:600px;max-width:600px;background-color:#FFFFFF;">
<tr><td style="background-color:#1F2937;padding:22px 32px;">
  <div style="font-family:Arial,Helvetica,sans-serif;font-size:20px;font-weight:bold;color:#FFFFFF;">{{.Site}}</div>
  <div style="font-family:Arial,Helvetica,sans-serif;font-size:11px;font-weight:bold;letter-spacing:1px;color:#BFDBFE;text-transform:uppercase;padding-top:6px;">{{.Eyebrow}}</div>
</td></tr>
<tr><td style="background-color:#1D4ED8;font-size:0;line-height:0;height:4px;">&nbsp;</td></tr>
<tr><td style="padding:28px 32px 8px 32px;font-family:Arial,Helvetica,sans-serif;color:#111827;">{{end}}

{{define "label"}}<div style="font-family:Arial,Helvetica,sans-serif;font-size:11px;font-weight:bold;letter-spacing:1px;text-transform:uppercase;color:#1D4ED8;padding:22px 0 8px 0;">{{.}}</div>{{end}}

{{define "foot"}}</td></tr>
<tr><td style="padding:8px 32px 28px 32px;font-family:Arial,Helvetica,sans-serif;">
  <div style="border-top:1px solid #D1D5DB;font-size:13.5px;line-height:1.65;color:#4B5563;padding-top:16px;">{{.Closing}}</div>
  {{with .SignatureName}}<div style="font-size:14px;font-weight:bold;color:#111827;padding-top:14px;">{{.}}</div>{{end}}
  {{with .SignatureRole}}<div style="font-size:12.5px;color:#4B5563;">{{.}}</div>{{end}}
  {{with .PrivacyURL}}<div style="font-size:12px;line-height:1.6;color:#4B5563;padding-top:12px;"><a href="{{.}}" style="color:#1D4ED8;">Privacy notice</a></div>{{end}}
</td></tr>
<tr><td style="background-color:#1F2937;padding:14px 32px;font-family:Arial,Helvetica,sans-serif;font-size:12px;color:#D1D5DB;">Minutes and analysis drafted with AI from the meeting transcript.</td></tr>
</table></td></tr></table></body></html>{{end}}`

const minutesEmailTmpl = `{{template "head" .}}
<div style="font-size:20px;font-weight:bold;line-height:1.3;color:#111827;">{{.Room}}</div>
<div style="font-size:13.5px;line-height:1.65;color:#4B5563;padding-top:4px;">{{.When}}{{with .Metrics}} · {{dur .DurationSeconds}}{{end}}</div>
<div style="font-size:14.5px;line-height:1.7;color:#111827;padding-top:18px;">Hello {{.Name}},</div>
<div style="font-size:14.5px;line-height:1.7;color:#111827;padding-top:8px;">Thank you for joining. Here are the minutes and the full transcript (attached). The minutes were drafted with AI from what was said in the meeting and reviewed by {{.Reviewer}} before sending. If anything is inaccurate, reply to this e-mail.</div>

{{template "label" "Summary"}}
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
  <td width="4" style="width:4px;background-color:#1D4ED8;font-size:0;line-height:0;">&nbsp;</td>
  <td style="background-color:#F3F4F6;padding:14px 16px;font-size:14.5px;line-height:1.7;color:#111827;">{{.R.Summary}}</td>
</tr></table>

{{if .R.KeyPoints}}{{template "label" "Key points"}}
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">{{range .R.KeyPoints}}
<tr><td width="18" valign="top" style="width:18px;padding:8px 0 0 0;"><div style="width:7px;height:7px;background-color:#1D4ED8;font-size:0;line-height:0;">&nbsp;</div></td>
<td style="font-size:14.5px;line-height:1.7;color:#111827;padding:2px 0;">{{.}}</td></tr>{{end}}
</table>{{end}}

{{if .R.Decisions}}{{template "label" "Decisions"}}
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">{{range .R.Decisions}}
<tr><td style="font-size:14.5px;line-height:1.7;color:#111827;padding:3px 0;"><strong>{{.Decision}}</strong>{{with .Context}}<span style="color:#4B5563;"> · {{.}}</span>{{end}}</td></tr>{{end}}
</table>{{end}}

{{if .R.ActionItems}}{{template "label" "Action items"}}
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="font-size:13.5px;line-height:1.5;">
<tr style="background-color:#1F2937;color:#FFFFFF;font-weight:bold;">
  <td style="padding:8px 10px;">Task</td><td style="padding:8px 10px;">Owner</td><td style="padding:8px 10px;">Due</td><td style="padding:8px 10px;">Confidence</td></tr>
{{range $i, $t := .R.ActionItems}}<tr style="background-color:{{if odd $i}}#F3F4F6{{else}}#FFFFFF{{end}};color:#111827;">
  <td style="padding:8px 10px;">{{$t.Task}}</td>
  <td style="padding:8px 10px;">{{with $t.Owner}}{{deref .}}{{else}}<span style="color:#4B5563;">unassigned</span>{{end}}</td>
  <td style="padding:8px 10px;white-space:nowrap;">{{with $t.DueDate}}{{deref .}}{{else}}<span style="color:#4B5563;">no date</span>{{end}}</td>
  <td style="padding:8px 10px;">{{$t.Confidence}}</td></tr>{{end}}
</table>{{end}}

{{template "label" "Sentiment analysis"}}
<div><span style="{{pill .R.Sentiment.Overall}}">{{.R.Sentiment.Overall}}</span></div>
<div style="font-size:14.5px;line-height:1.7;color:#111827;padding-top:8px;">{{.R.Sentiment.Explanation}}</div>

{{if .R.Sentiment.Timeline}}<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin-top:12px;"><tr>
{{range .R.Sentiment.Timeline}}<td valign="top" style="background-color:#F3F4F6;padding:10px 12px;border-right:4px solid #FFFFFF;">
  <div style="font-size:11px;font-weight:bold;letter-spacing:1px;text-transform:uppercase;color:#1D4ED8;">{{phase .Phase}}</div>
  <div style="padding:6px 0;"><span style="{{pill .Overall}}">{{.Overall}}</span></div>
  <div style="font-size:13px;line-height:1.55;color:#111827;">{{.Summary}}</div></td>{{end}}
</tr></table>{{end}}

{{if .R.Sentiment.BySpeaker}}<div style="font-size:13.5px;font-weight:bold;color:#111827;padding:16px 0 6px 0;">By participant</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">{{range .R.Sentiment.BySpeaker}}
<tr><td style="padding:6px 0;font-size:13.5px;line-height:1.6;color:#111827;"><strong>{{.Speaker}}</strong> <span style="{{pill .Overall}}">{{.Overall}}</span><br>{{.Explanation}}{{with .Evidence}}<br><span style="color:#4B5563;font-style:italic;">“{{.}}”</span>{{end}}</td></tr>{{end}}
</table>{{end}}

{{if .R.Sentiment.Moments}}<div style="font-size:13.5px;font-weight:bold;color:#111827;padding:14px 0 6px 0;">Key moments</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">{{range .R.Sentiment.Moments}}
<tr><td style="padding:5px 0;font-size:13.5px;line-height:1.6;color:#111827;"><span style="color:#1D4ED8;font-weight:bold;text-transform:uppercase;font-size:11px;letter-spacing:1px;">{{.Emotion}}</span> · {{with .Time}}{{deref .}} · {{end}}<strong>{{.Speaker}}</strong>: <span style="font-style:italic;">“{{.Quote}}”</span></td></tr>{{end}}
</table>{{end}}

{{with .Metrics}}<div style="font-size:13.5px;font-weight:bold;color:#111827;padding:16px 0 6px 0;">Participation, {{if .Measured}}measured from the audio{{else}}estimated{{end}}</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="font-size:12.5px;line-height:1.5;color:#111827;">{{range .Speakers}}
<tr><td width="130" style="width:130px;padding:5px 8px 5px 0;font-weight:bold;">{{.Speaker}}</td>
<td style="padding:5px 0;"><table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
  <td width="{{pct .SharePct}}%" style="background-color:#1D4ED8;font-size:0;line-height:0;height:10px;">&nbsp;</td>
  <td width="{{sub 100 (pct .SharePct)}}%" style="background-color:#F3F4F6;font-size:0;line-height:0;height:10px;">&nbsp;</td></tr></table>
  <div style="color:#4B5563;padding-top:3px;">{{printf "%.0f" .SharePct}}% of talk time · {{printf "%.0f" .WordsPerMinute}} words/min · {{.Turns}} turns{{if .Interruptions}} · {{.Interruptions}} interruptions{{end}}</div></td></tr>{{end}}
</table>{{end}}
<div style="font-size:12px;line-height:1.6;color:#4B5563;padding-top:10px;">{{with .R.Sentiment.Caveats}}{{.}} {{end}}The analysis combines what was said with signals measured from the audio (talk time, pace, turns and interruptions); it does not measure tone of voice.</div>

{{if .R.Risks}}{{template "label" "Risks"}}
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
  <td width="4" style="width:4px;background-color:#B91C1C;font-size:0;line-height:0;">&nbsp;</td>
  <td style="background-color:#F3F4F6;padding:12px 16px;font-size:14px;line-height:1.7;color:#111827;">{{range $i, $r := .R.Risks}}{{if $i}}<br>{{end}}· {{$r}}{{end}}</td>
</tr></table>{{end}}

{{if .R.OpenQuestions}}{{template "label" "Open questions"}}
<div style="font-size:14px;line-height:1.7;color:#111827;">{{range $i, $q := .R.OpenQuestions}}{{if $i}}<br>{{end}}· {{$q}}{{end}}</div>{{end}}

<div style="font-size:13px;line-height:1.6;color:#4B5563;padding-top:22px;">Attachments: the full transcript (.txt) and the minutes (.md).</div>
{{template "foot" .}}`

const hostNoticeTmpl = `{{template "head" .}}
<div style="font-size:20px;font-weight:bold;line-height:1.3;color:#111827;">Your minutes are ready for review</div>
<div style="font-size:14.5px;line-height:1.7;color:#111827;padding-top:12px;">The <strong>{{.Room}}</strong> meeting on {{.When}} now has a transcript and minutes.</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin-top:16px;"><tr>
  <td width="4" style="width:4px;background-color:#1D4ED8;font-size:0;line-height:0;">&nbsp;</td>
  <td style="background-color:#F3F4F6;padding:14px 16px;font-size:14.5px;line-height:1.7;color:#111827;">{{.Pending}}</td>
</tr></table>
<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="margin-top:22px;"><tr>
  <td style="background-color:#1D4ED8;border-radius:4px;">
    <a href="{{.Link}}" style="display:inline-block;padding:14px 32px;font-family:Arial,Helvetica,sans-serif;font-size:14px;font-weight:bold;color:#FFFFFF;text-decoration:none;">Review and approve</a></td>
</tr></table>
<div style="font-size:13px;line-height:1.6;color:#4B5563;padding-top:16px;">Nothing is sent to participants until you approve it in the dashboard. This notice contains no meeting content.</div>
{{template "foot" .}}`

var (
	minutesEmail = template.Must(template.New("minutes").Funcs(emailFuncs).Parse(emailFrame + minutesEmailTmpl))
	hostEmail    = template.Must(template.New("host").Funcs(emailFuncs).Parse(emailFrame + hostNoticeTmpl))
)

type minutesEmailData struct {
	Subject, Preheader, Eyebrow, Closing string
	Site, PrivacyURL                     string
	SignatureName, SignatureRole         string
	Room, When, Name, Reviewer           string
	R                                    *MinutesResult
	Metrics                              *ConversationMetrics
}

type hostEmailData struct {
	Subject, Preheader, Eyebrow, Closing string
	Site, PrivacyURL                     string
	SignatureName, SignatureRole         string
	Room, When, Pending, Link            string
}

func renderEmail(t *template.Template, data any) (string, error) {
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

func firstSentence(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, ".!?"); i > 0 && i < max {
		return s[:i+1]
	}
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// meetingWhen formats the meeting start for people, with the time zone the
// deployment displays (DISPLAY_TIMEZONE).
func meetingWhen(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("January 2, 2006, 15:04") + " (" + loc.String() + ")"
}
