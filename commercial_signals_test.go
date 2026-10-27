package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func commercialFixture(t *testing.T, signals any) []byte {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(validMinutesJSON), &raw); err != nil {
		t.Fatal(err)
	}
	raw["commercial_signals"] = signals
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sampleCommercialSignal() CommercialSignal {
	return CommercialSignal{
		Kind: "new_opportunity", Intent: "explicit_need", Service: "Integración",
		Speaker: "Cliente", Evidence: "Necesitamos conectar el inventario.",
		Reason:   "Existe una necesidad expresa; alcance y comprador por confirmar.",
		NextStep: "Preguntar por el alcance y quién valida la necesidad.", Confidence: "medium",
	}
}

func TestCommercialSchemaLegacyAndEmptyCoverage(t *testing.T) {
	old, err := validateMinutes([]byte(validMinutesJSON))
	if err != nil || old.CommercialAnalysisAvailable() {
		t.Fatalf("legacy coverage: %v", err)
	}
	current, err := validateMinutes(commercialFixture(t, []CommercialSignal{}))
	if err != nil || !current.CommercialAnalysisAvailable() || len(current.CommercialSignals) != 0 {
		t.Fatalf("empty analyzed coverage: %v", err)
	}
	if _, err := validateMinutes(commercialFixture(t, nil)); err == nil {
		t.Fatal("null signals accepted")
	}
	var schema struct{ Required []string }
	if err := json.Unmarshal([]byte(minutesSchema), &schema); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, field := range schema.Required {
		found = found || field == "commercial_signals"
	}
	if !found {
		t.Fatal("new provider schema does not require commercial analysis")
	}
}

func TestCommercialSignalsRejectInvalidAndDuplicateClaims(t *testing.T) {
	valid := sampleCommercialSignal()
	if _, err := validateMinutes(commercialFixture(t, []CommercialSignal{valid})); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*CommercialSignal){
		"unbounded_service":       func(s *CommercialSignal) { s.Service = strings.Repeat("x", 201) },
		"no_evidence":             func(s *CommercialSignal) { s.Evidence = "" },
		"no_speaker":              func(s *CommercialSignal) { s.Speaker = " " },
		"no_reason":               func(s *CommercialSignal) { s.Reason = "" },
		"no_next_step":            func(s *CommercialSignal) { s.NextStep = "" },
		"invented_kind":           func(s *CommercialSignal) { s.Kind = "guaranteed_sale" },
		"probability":             func(s *CommercialSignal) { s.Confidence = "90%" },
		"sentiment_is_not_intent": func(s *CommercialSignal) { s.Intent = "positive" },
	} {
		t.Run(name, func(t *testing.T) {
			s := valid
			change(&s)
			if _, err := validateMinutes(commercialFixture(t, []CommercialSignal{s})); err == nil {
				t.Fatal("invalid claim accepted")
			}
		})
	}
	if _, err := validateMinutes(commercialFixture(t, []CommercialSignal{valid, valid})); err == nil {
		t.Fatal("duplicate hypotheses inflate counts")
	}
	secondService := valid
	secondService.Service = "Tienda web"
	if _, err := validateMinutes(commercialFixture(t, []CommercialSignal{valid, secondService})); err != nil {
		t.Fatalf("two services from the same statement are distinct hypotheses: %v", err)
	}
	tooMany := make([]CommercialSignal, 13)
	if _, err := validateMinutes(commercialFixture(t, tooMany)); err == nil {
		t.Fatal("unbounded hypothesis count")
	}
	var raw map[string]any
	b, _ := json.Marshal(valid)
	_ = json.Unmarshal(b, &raw)
	raw["service"] = nil
	if _, err := validateMinutes(commercialFixture(t, []any{raw})); err == nil {
		t.Fatal("null field accepted")
	}
}

func TestCommercialEvidenceRequiresExactQuoteAndSpeaker(t *testing.T) {
	s := sampleCommercialSignal()
	transcript := `{"messages":[{"name":"Cliente","text":"Necesitamos conectar el inventario.","timestamp":1793059200000}]}`
	r := &MinutesResult{CommercialSignals: []CommercialSignal{s}}
	if err := validateEvidence(r, transcript); err != nil {
		t.Fatal(err)
	}
	r.CommercialSignals[0].Evidence = "Tenemos presupuesto aprobado."
	if err := validateEvidence(r, transcript); err == nil {
		t.Fatal("fabricated evidence accepted")
	}
	r.CommercialSignals[0] = s
	r.CommercialSignals[0].Speaker = "Director"
	if err := validateEvidence(r, transcript); err == nil {
		t.Fatal("invented speaker accepted")
	}
}

func TestCommercialHypothesesRemainInternal(t *testing.T) {
	r, err := validateMinutes(commercialFixture(t, []CommercialSignal{sampleCommercialSignal()}))
	if err != nil {
		t.Fatal(err)
	}
	md := minutesMarkdown(&Minutes{RoomSlug: "sala-prueba", MeetingAt: time.Now()}, r, time.UTC)
	if strings.Contains(md, "Necesitamos conectar el inventario.") || strings.Contains(md, "Hipótesis") {
		t.Fatal("internal commercial hypotheses leaked into participant Markdown")
	}
	e := newTestEnv(t)
	browser, csrf := e.loginAdmin()
	e.createRoom(browser, csrf, "sala-comercial")
	e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "commercial-event", "session", "sala-comercial", testLink))
	drain(e)
	ms, err := e.app.DB.ListMinutes("sala-comercial")
	if err != nil || len(ms) != 1 {
		t.Fatalf("minutes: %v", err)
	}
	data, _ := json.Marshal(r)
	if _, err := e.app.DB.conn.Exec("UPDATE minutes SET result_json=? WHERE id=?", string(data), ms[0].ID); err != nil {
		t.Fatal(err)
	}
	mail, err := e.app.composeDelivery(&delivery{MinutesID: ms[0].ID, Email: "participant@example.test", Name: "Invitado", ApprovedBy: "owner@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{mail.Text, mail.HTML} {
		if strings.Contains(body, "Necesitamos conectar el inventario.") || strings.Contains(body, "commercial_signals") {
			t.Fatal("internal commercial hypotheses leaked into participant email")
		}
	}
	for _, attachment := range mail.Attachments {
		if strings.Contains(string(attachment.Data), "Necesitamos conectar el inventario.") {
			t.Fatal("internal commercial hypotheses leaked into participant attachment")
		}
	}
	page := browser.do(http.MethodGet, "/admin/minutes/"+itoa(ms[0].ID), nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Señales comerciales para revisión interna") ||
		!strings.Contains(page.Body.String(), "Necesitamos conectar el inventario.") {
		t.Fatalf("internal UI missing: %d", page.Code)
	}
	if anonymous := e.browser().do(http.MethodGet, "/admin/minutes/"+itoa(ms[0].ID), nil); anonymous.Code == http.StatusOK {
		t.Fatal("internal intelligence accessible anonymously")
	}
}
