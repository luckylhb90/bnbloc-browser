package main

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
)

// The decision model's questions (a port of jev-ultrafast's model.py,
// NOTICE): bounded ones only, since the agent drives (agent.go). Whether a
// chosen click can be undone (irreversibleRequest) and which control a
// description names (find) are SystemOne questions about indices the code
// assigned, never selectors.

// Operation names, as runs record them and checkpoints hold them.
const (
	opClick  = "CLICK"
	opUpload = "UPLOAD"
)

// irreversibleRule is the predicate asked about a chosen click.
const irreversibleRule = `Would this click submit, pay, send, delete, publish, book, order or otherwise commit something that cannot be undone, or that other people or systems receive? Opening, navigating, filtering, searching, selecting, expanding and editing a draft are reversible.`

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// choiceAnswer is one SystemOne choice answer.
type choiceAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Confidence    *float64           `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// errInvalidAnswer: a decision that does not answer what was asked; no
// action follows from it.
var errInvalidAnswer = errors.New("the decision model's answer does not match the question; no action executed")

// decision is one step's validated choice.
type decision struct {
	Operation  string
	Target     string // the target head's index ("" for a control, DONE, BLOCKED)
	Action     action // the action to run (zero for DONE / BLOCKED)
	Confidence float64
	Usage      json.RawMessage
}

// irreversibleRequest asks one predicate about a chosen click: the element
// and its form/dialog scope (the snapshot's guard scope), never the page.
func irreversibleRequest(model, goal string, a action, scope string) map[string]any {
	return map[string]any{
		"model": model,
		"state": map[string]any{"goal": goal, "element": map[string]any{"label": a.Label, "role": a.Role, "href": a.Href}, "context": scope},
		"questions": map[string]any{
			"irreversible": map[string]any{"type": "noul", "instructions": irreversibleRule},
		},
	}
}

// readIrreversible reads the predicate's probability of yes.
func readIrreversible(raw []byte) (float64, error) {
	var reply struct {
		Answers map[string]struct {
			Type string   `json:"type"`
			Noul *float64 `json:"noul"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return 0, errInvalidAnswer
	}
	a := reply.Answers["irreversible"]
	if a.Type == "refusal" {
		return 1, nil // a declined judgement pauses, like a yes
	}
	if a.Noul == nil || math.IsNaN(*a.Noul) || *a.Noul < 0 || *a.Noul > 1 {
		return 0, errInvalidAnswer
	}
	return *a.Noul, nil
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

// orderedMap is a JSON object written in insertion order: the questions'
// and criteria's written order is the order the model reads them.
type orderedMap struct {
	keys []string
	vals map[string]any
}

func (m *orderedMap) set(k string, v any) {
	if m.vals == nil {
		m.vals = map[string]any{}
	}
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
}

func (m orderedMap) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kr, _ := json.Marshal(k)
		vr, err := json.Marshal(m.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(kr)
		b.WriteByte(':')
		b.Write(vr)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}
