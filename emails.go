package main

import (
	"bytes"
	"html/template"
	"strings"
	"time"
)

// HTML e-mails follow the Transgenia style sheet 2026 and the e-mail rules
// of the brand's mailing reference: 600 px nested tables, inline CSS only,
// no remote images or web fonts, navy header with the wordmark, the official
// orange-to-crimson gradient bar (solid fallback for Outlook), crimson as the
// text accent on light backgrounds (orange fails contrast there) and a navy
// closing band. Every text/background pair passes WCAG AA
// (verificar_contraste.py): sentiment pills use 548C, Black 7C, 1797C and
// Black 6C with cream text.

const (
	cNavy    = "#101820"
	cOrange  = "#EE7623"
	cCrimson = "#D1333B"
	cCream   = "#FEFEEA"
	cPetrol  = "#003C4D"
	cWarm    = "#3D3935"
)

// GEN analysing a meeting (tools/gen_analiza.py), embedded in the message
// as a cid: image, never loaded from the network. Clients that block images
// show the alt text; Outlook desktop shows the first frame.
const genContentID = "gen-analiza@transgenia.org"

func genInline() []Attachment {
	data, err := staticFS.ReadFile("static/gen-analiza.gif")
	if err != nil {
		return nil
	}
	return []Attachment{{Name: "gen-analiza.gif", ContentType: "image/gif", Data: data, ContentID: genContentID}}
}

const genImg = `<img src="cid:gen-analiza@transgenia.org" width="168" height="84" alt="GEN, la mascota de Transgenia, analizando la reunión" style="display:block;border:0;outline:none;text-decoration:none;width:168px;height:84px;">`

var emailFuncs = template.FuncMap{
	"gen": func() template.HTML { return template.HTML(genImg) },
	"pill": func(v string) template.CSS {
		bg := cNavy
		switch v {
		case "positivo":
			bg = cPetrol
		case "neutral":
			bg = cWarm
		case "negativo":
			bg = cCrimson
		}
		return template.CSS("display:inline-block;background-color:" + bg + ";color:" + cCream +
			";font-family:Inter,Arial,Helvetica,sans-serif;font-size:11px;font-weight:bold;letter-spacing:1px;text-transform:uppercase;padding:3px 9px;border-radius:10px;")
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
		case "inicio":
			return "Inicio"
		case "desarrollo":
			return "Desarrollo"
		}
		return "Cierre"
	},
	"conf": func(c string) string {
		switch c {
		case "high":
			return "alta"
		case "medium":
			return "media"
		}
		return "baja"
	},
	"odd": func(i int) bool { return i%2 == 1 },
}

// Shared frame. Blocks: preheader, navy header, gradient bar, body, closing band.
const emailFrame = `{{define "head"}}<!doctype html>
<html lang="es"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Subject}}</title></head>
<body style="margin:0;padding:0;background-color:#FEFEEA;">
<div style="display:none;max-height:0;overflow:hidden;opacity:0;">{{.Preheader}}</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color:#FEFEEA;">
<tr><td align="center" style="padding:24px 12px;">
<table role="presentation" width="600" cellpadding="0" cellspacing="0" border="0" style="width:600px;max-width:600px;background-color:#FFFFFF;">
<tr><td style="background-color:#101820;padding:24px 32px;">
  <div style="font-family:'Garnet Capitals',Inter,Arial,Helvetica,sans-serif;font-size:24px;font-weight:bold;letter-spacing:3px;color:#FEFEEA;">TRANSGENIA</div>
  <div style="font-family:Inter,Arial,Helvetica,sans-serif;font-size:11px;font-weight:bold;letter-spacing:1px;color:#EE7623;text-transform:uppercase;padding-top:6px;">{{.Eyebrow}}</div>
</td></tr>
<tr><td style="background-color:#EE7623;background-image:linear-gradient(90deg,#EE7623 0%,#F0A055 33%,#D8292F 72%,#D1333B 100%);font-size:0;line-height:0;height:5px;">&nbsp;</td></tr>
<tr><td style="padding:28px 32px 8px 32px;font-family:Inter,Arial,Helvetica,sans-serif;color:#101820;">{{end}}

{{define "label"}}<div style="font-family:Inter,Arial,Helvetica,sans-serif;font-size:11px;font-weight:bold;letter-spacing:1px;text-transform:uppercase;color:#D1333B;padding:22px 0 8px 0;">{{.}}</div>{{end}}

{{define "foot"}}</td></tr>
<tr><td style="padding:8px 32px 28px 32px;font-family:Inter,Arial,Helvetica,sans-serif;">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr><td style="background-color:#EE7623;background-image:linear-gradient(90deg,#EE7623 0%,#F0A055 33%,#D8292F 72%,#D1333B 100%);font-size:0;line-height:0;height:2px;">&nbsp;</td></tr></table>
  <div style="font-size:13.5px;line-height:1.65;color:#3D3935;padding-top:16px;">{{.Closing}}</div>
  {{with .SignatureName}}<div style="font-size:14px;font-weight:bold;color:#101820;padding-top:14px;">{{.}}</div>{{end}}
  {{with .SignatureRole}}<div style="font-size:12.5px;color:#3D3935;">{{.}}</div>{{end}}
  <div style="font-size:11.5px;font-weight:bold;letter-spacing:2px;color:#D1333B;padding-top:12px;text-transform:uppercase;">Transgenia: IA aplicada</div>
  <div style="font-size:12px;line-height:1.6;color:#3D3935;padding-top:4px;"><a href="https://transgenia.org" style="color:#3D3935;text-decoration:none;">transgenia.org</a> · <a href="https://transgenia.org/legal-privacidad.html" style="color:#D1333B;">Aviso de privacidad</a></div>
</td></tr>
<tr><td style="background-color:#101820;padding:14px 32px;font-family:Inter,Arial,Helvetica,sans-serif;font-size:12px;color:#FEFEEA;">Transgenia: IA aplicada. Minuta y análisis generados con inteligencia artificial por Transgenia.</td></tr>
</table></td></tr></table></body></html>{{end}}`

const minutesEmailTmpl = `{{template "head" .}}
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
<td valign="middle" style="font-family:Inter,Arial,Helvetica,sans-serif;">
  <div style="font-size:20px;font-weight:bold;line-height:1.3;color:#101820;">{{.Room}}</div>
  <div style="font-size:13.5px;line-height:1.65;color:#3D3935;padding-top:4px;">{{.When}} (hora del centro de México){{with .Metrics}} · {{dur .DurationSeconds}}{{end}}</div>
</td>
<td width="168" align="right" valign="middle" style="width:168px;padding-left:12px;">{{gen}}</td>
</tr></table>
<div style="font-size:14.5px;line-height:1.7;color:#101820;padding-top:18px;">Hola {{.Name}}:</div>
<div style="font-size:14.5px;line-height:1.7;color:#101820;padding-top:8px;">Gracias por participar. Te compartimos la minuta y la transcripción completa (adjunta). La minuta se generó con apoyo de inteligencia artificial a partir de lo que se dijo en la reunión y {{.Reviewer}} la revisó antes de este envío. Si algo no es exacto, responde a este correo.</div>

{{template "label" "Resumen"}}
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
  <td width="4" style="width:4px;background-color:#EE7623;font-size:0;line-height:0;">&nbsp;</td>
  <td style="background-color:#FEFEEA;padding:14px 16px;font-size:14.5px;line-height:1.7;color:#101820;">{{.R.Summary}}</td>
</tr></table>

{{if .R.KeyPoints}}{{template "label" "Puntos clave"}}
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">{{range .R.KeyPoints}}
<tr><td width="18" valign="top" style="width:18px;padding:8px 0 0 0;"><div style="width:7px;height:7px;background-color:#EE7623;font-size:0;line-height:0;">&nbsp;</div></td>
<td style="font-size:14.5px;line-height:1.7;color:#101820;padding:2px 0;">{{.}}</td></tr>{{end}}
</table>{{end}}

{{if .R.Decisions}}{{template "label" "Decisiones"}}
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">{{range .R.Decisions}}
<tr><td style="font-size:14.5px;line-height:1.7;color:#101820;padding:3px 0;"><strong>{{.Decision}}</strong>{{with .Context}}<span style="color:#3D3935;"> · {{.}}</span>{{end}}</td></tr>{{end}}
</table>{{end}}

{{if .R.ActionItems}}{{template "label" "Tareas acordadas"}}
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="font-size:13.5px;line-height:1.5;">
<tr style="background-color:#101820;color:#FEFEEA;font-weight:bold;">
  <td style="padding:8px 10px;">Tarea</td><td style="padding:8px 10px;">Responsable</td><td style="padding:8px 10px;">Fecha</td><td style="padding:8px 10px;">Confianza</td></tr>
{{range $i, $t := .R.ActionItems}}<tr style="background-color:{{if odd $i}}#FEFEEA{{else}}#FFFFFF{{end}};color:#101820;">
  <td style="padding:8px 10px;">{{$t.Task}}</td>
  <td style="padding:8px 10px;">{{with $t.Owner}}{{deref .}}{{else}}<span style="color:#3D3935;">sin asignar</span>{{end}}</td>
  <td style="padding:8px 10px;white-space:nowrap;">{{with $t.DueDate}}{{deref .}}{{else}}<span style="color:#3D3935;">sin fecha</span>{{end}}</td>
  <td style="padding:8px 10px;">{{conf $t.Confidence}}</td></tr>{{end}}
</table>{{end}}

{{template "label" "Análisis de sentimiento"}}
<div><span style="{{pill .R.Sentiment.Overall}}">{{.R.Sentiment.Overall}}</span></div>
<div style="font-size:14.5px;line-height:1.7;color:#101820;padding-top:8px;">{{.R.Sentiment.Explanation}}</div>

{{if .R.Sentiment.Timeline}}<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin-top:12px;"><tr>
{{range .R.Sentiment.Timeline}}<td valign="top" style="background-color:#FEFEEA;padding:10px 12px;border-right:4px solid #FFFFFF;">
  <div style="font-size:11px;font-weight:bold;letter-spacing:1px;text-transform:uppercase;color:#D1333B;">{{phase .Phase}}</div>
  <div style="padding:6px 0;"><span style="{{pill .Overall}}">{{.Overall}}</span></div>
  <div style="font-size:13px;line-height:1.55;color:#101820;">{{.Summary}}</div></td>{{end}}
</tr></table>{{end}}

{{if .R.Sentiment.BySpeaker}}<div style="font-size:13.5px;font-weight:bold;color:#101820;padding:16px 0 6px 0;">Por participante</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">{{range .R.Sentiment.BySpeaker}}
<tr><td style="padding:6px 0;font-size:13.5px;line-height:1.6;color:#101820;"><strong>{{.Speaker}}</strong> <span style="{{pill .Overall}}">{{.Overall}}</span><br>{{.Explanation}}{{with .Evidence}}<br><span style="color:#3D3935;font-style:italic;">“{{.}}”</span>{{end}}</td></tr>{{end}}
</table>{{end}}

{{if .R.Sentiment.Moments}}<div style="font-size:13.5px;font-weight:bold;color:#101820;padding:14px 0 6px 0;">Momentos clave</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">{{range .R.Sentiment.Moments}}
<tr><td style="padding:5px 0;font-size:13.5px;line-height:1.6;color:#101820;"><span style="color:#D1333B;font-weight:bold;text-transform:uppercase;font-size:11px;letter-spacing:1px;">{{.Emotion}}</span> · {{with .Time}}{{deref .}} · {{end}}<strong>{{.Speaker}}</strong>: <span style="font-style:italic;">“{{.Quote}}”</span></td></tr>{{end}}
</table>{{end}}

{{with .Metrics}}<div style="font-size:13.5px;font-weight:bold;color:#101820;padding:16px 0 6px 0;">Participación {{if .Measured}}medida en el audio{{else}}estimada{{end}}</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="font-size:12.5px;line-height:1.5;color:#101820;">{{range .Speakers}}
<tr><td width="130" style="width:130px;padding:5px 8px 5px 0;font-weight:bold;">{{.Speaker}}</td>
<td style="padding:5px 0;"><table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
  <td width="{{pct .SharePct}}%" style="background-color:#EE7623;font-size:0;line-height:0;height:10px;">&nbsp;</td>
  <td width="{{sub 100 (pct .SharePct)}}%" style="background-color:#FEFEEA;font-size:0;line-height:0;height:10px;">&nbsp;</td></tr></table>
  <div style="color:#3D3935;padding-top:3px;">{{printf "%.0f" .SharePct}}% del tiempo · {{printf "%.0f" .WordsPerMinute}} palabras/min · {{.Turns}} turnos{{if .Interruptions}} · {{.Interruptions}} interrupciones{{end}}</div></td></tr>{{end}}
</table>{{end}}
<div style="font-size:12px;line-height:1.6;color:#3D3935;padding-top:10px;">{{with .R.Sentiment.Caveats}}{{.}} {{end}}El análisis combina lo dicho con señales medidas del audio (tiempo de habla, ritmo, turnos e interrupciones); no mide el tono de voz.</div>

{{if .R.Risks}}{{template "label" "Riesgos"}}
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
  <td width="4" style="width:4px;background-color:#D1333B;font-size:0;line-height:0;">&nbsp;</td>
  <td style="background-color:#FEFEEA;padding:12px 16px;font-size:14px;line-height:1.7;color:#101820;">{{range $i, $r := .R.Risks}}{{if $i}}<br>{{end}}· {{$r}}{{end}}</td>
</tr></table>{{end}}

{{if .R.OpenQuestions}}{{template "label" "Preguntas abiertas"}}
<div style="font-size:14px;line-height:1.7;color:#101820;">{{range $i, $q := .R.OpenQuestions}}{{if $i}}<br>{{end}}· {{$q}}{{end}}</div>{{end}}

<div style="font-size:13px;line-height:1.6;color:#3D3935;padding-top:22px;">Adjuntos: la transcripción completa (.txt) y la minuta (.md).</div>
{{template "foot" .}}`

const hostNoticeTmpl = `{{template "head" .}}
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
<td valign="middle" style="font-family:Inter,Arial,Helvetica,sans-serif;font-size:20px;font-weight:bold;line-height:1.3;color:#101820;">Tu minuta está lista para revisar</td>
<td width="168" align="right" valign="middle" style="width:168px;padding-left:12px;">{{gen}}</td>
</tr></table>
<div style="font-size:14.5px;line-height:1.7;color:#101820;padding-top:12px;">La reunión <strong>{{.Room}}</strong> del {{.When}} (hora del centro de México) ya tiene transcripción y minuta.</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin-top:16px;"><tr>
  <td width="4" style="width:4px;background-color:#EE7623;font-size:0;line-height:0;">&nbsp;</td>
  <td style="background-color:#FEFEEA;padding:14px 16px;font-size:14.5px;line-height:1.7;color:#101820;">{{.Pending}}</td>
</tr></table>
<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="margin-top:22px;"><tr>
  <td style="background-color:#101820;border-radius:4px;border-bottom:3px solid #EE7623;">
    <a href="{{.Link}}" style="display:inline-block;padding:14px 32px;font-family:Inter,Arial,Helvetica,sans-serif;font-size:14px;font-weight:bold;color:#FEFEEA;text-decoration:none;">Revisar y aprobar</a></td>
</tr></table>
<div style="font-size:13px;line-height:1.6;color:#3D3935;padding-top:16px;">Nada se envía a los participantes hasta que lo apruebes en el panel. Este aviso no incluye contenido de la reunión.</div>
{{template "foot" .}}`

var (
	minutesEmail = template.Must(template.New("minutes").Funcs(emailFuncs).Parse(emailFrame + minutesEmailTmpl))
	hostEmail    = template.Must(template.New("host").Funcs(emailFuncs).Parse(emailFrame + hostNoticeTmpl))
)

type minutesEmailData struct {
	Subject, Preheader, Eyebrow, Closing string
	SignatureName, SignatureRole         string
	Room, When, Name, Reviewer           string
	R                                    *MinutesResult
	Metrics                              *ConversationMetrics
}

type hostEmailData struct {
	Subject, Preheader, Eyebrow, Closing string
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

func meetingWhen(t time.Time, loc *time.Location) string {
	months := []string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"}
	l := t.In(loc)
	return l.Format("2") + " de " + months[l.Month()-1] + " de " + l.Format("2006, 15:04")
}
