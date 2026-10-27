package main

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

// CommercialSignal is an internal, evidence-backed hypothesis, not a confirmed
// opportunity or a measurement of a person's emotions or likelihood to buy.
type CommercialSignal struct {
	Kind       string `json:"kind"`
	Intent     string `json:"intent"`
	Service    string `json:"service"`
	Evidence   string `json:"evidence"`
	Speaker    string `json:"speaker"`
	Reason     string `json:"reason"`
	NextStep   string `json:"next_step"`
	Confidence string `json:"confidence"`
}

func (r MinutesResult) CommercialAnalysisAvailable() bool {
	return r.CommercialSignals != nil
}

func validateCommercialJSON(raw json.RawMessage) error {
	var rows []map[string]json.RawMessage
	if json.Unmarshal(raw, &rows) != nil || rows == nil || len(rows) > 12 {
		return errors.New("esquema inválido: commercial_signals debe ser una lista de hasta 12 hipótesis")
	}
	seen := make(map[string]bool)
	for _, row := range rows {
		if err := requireKeys(row, "commercial_signals", "kind", "intent", "service", "evidence", "speaker", "reason", "next_step", "confidence"); err != nil {
			return err
		}
		// null is not an empty string: reject it before unmarshalling into Go.
		for _, value := range row {
			var s any
			if json.Unmarshal(value, &s) != nil {
				return errors.New("esquema inválido: señal comercial")
			}
			if _, ok := s.(string); !ok {
				return errors.New("esquema inválido: campos comerciales deben ser texto")
			}
		}
		b, _ := json.Marshal(row)
		var s CommercialSignal
		if json.Unmarshal(b, &s) != nil {
			return errors.New("esquema inválido: señal comercial")
		}
		switch s.Kind {
		case "renewal", "upsell", "cross_sell", "new_opportunity", "retention_risk":
		default:
			return errors.New("esquema inválido: tipo de señal comercial")
		}
		switch s.Intent {
		case "exploring", "explicit_need", "quote_requested", "objection":
		default:
			return errors.New("esquema inválido: intención comercial")
		}
		switch s.Confidence {
		case "high", "medium", "low":
		default:
			return errors.New("esquema inválido: confianza comercial")
		}
		for _, field := range []struct {
			text     string
			max      int
			optional bool
		}{
			{s.Service, 200, true}, {s.Speaker, 200, false},
			{s.Evidence, 1000, false}, {s.Reason, 1000, false},
			{s.NextStep, 1000, false},
		} {
			if (!field.optional && strings.TrimSpace(field.text) == "") || utf8.RuneCountInString(field.text) > field.max {
				return errors.New("esquema inválido: texto comercial vacío o demasiado largo")
			}
		}
		key := s.Kind + "\x00" + s.Intent + "\x00" + strings.ToLower(normalEvidence(s.Service)) + "\x00" + normalEvidence(s.Evidence) + "\x00" + s.Speaker
		if seen[key] {
			return errors.New("esquema inválido: señal comercial duplicada")
		}
		seen[key] = true
	}
	return nil
}

func (s CommercialSignal) KindLabel() string {
	switch s.Kind {
	case "renewal":
		return "Recompra o renovación"
	case "upsell":
		return "Ampliación de servicio"
	case "cross_sell":
		return "Servicio complementario"
	case "new_opportunity":
		return "Nueva necesidad"
	case "retention_risk":
		return "Riesgo de continuidad"
	}
	return s.Kind
}

func (s CommercialSignal) IntentLabel() string {
	switch s.Intent {
	case "exploring":
		return "Exploración"
	case "explicit_need":
		return "Necesidad explícita"
	case "quote_requested":
		return "Solicitud de cotización"
	case "objection":
		return "Objeción"
	}
	return s.Intent
}
