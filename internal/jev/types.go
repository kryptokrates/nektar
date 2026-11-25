package jev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
)

const (
	ProviderTypeSafe   = "typesafe"
	ProviderOpenRouter = "openrouter"

	DefaultTypeSafeURL     = "https://api.typesafe.ai/v1/systemone"
	DefaultOpenRouterURL   = "https://openrouter.ai/api/alpha/decisions"
	DefaultTypeSafeModel   = "jev-latest"
	DefaultOpenRouterModel = "typesafe/jev-1.14"
)

// QuestionType identifies one of Jev AI's System One structured decision primitives.
type QuestionType string

const (
	TypeNoul   QuestionType = "noul"
	TypeChoice QuestionType = "choice"
	TypeScore  QuestionType = "score"
)

// Question declares one typed evaluation question submitted in parallel against State.
type Question struct {
	Type         QuestionType `json:"type"`
	Instructions string       `json:"instructions"`
	// Criteria is map[string]string for choice/noul or []string (2..10 ordered rubric levels) for score.
	Criteria any `json:"criteria,omitempty"`
}

// Request is the System One API payload evaluated in a single parallel forward pass.
type Request struct {
	Model     string              `json:"model"`
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// ProbabilityMap unmarshals either a JSON object ({"0": 0.1, "1": 0.9}) or JSON array ([0.1, 0.9]).
type ProbabilityMap map[string]float64

func (p *ProbabilityMap) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*p = nil
		return nil
	}
	var obj map[string]float64
	if err := json.Unmarshal(data, &obj); err == nil {
		*p = obj
		return nil
	}
	var arr []float64
	if err := json.Unmarshal(data, &arr); err == nil {
		out := make(map[string]float64, len(arr))
		for i, v := range arr {
			out[strconv.Itoa(i)] = v
		}
		*p = out
		return nil
	}
	return errors.New("jev: probabilities must be JSON object or float array")
}

// Answer is one typed decision returned by Jev AI, normalized across TypeSafe direct and OpenRouter dialects.
type Answer struct {
	Type          QuestionType   `json:"type"`
	Noul          *float64       `json:"noul,omitempty"`        // TypeSafe direct dialect
	Probability   *float64       `json:"probability,omitempty"` // OpenRouter dialect
	Choice        string         `json:"choice,omitempty"`
	Score         *float64       `json:"score,omitempty"`
	Confidence    *float64       `json:"confidence,omitempty"`
	Probabilities ProbabilityMap `json:"probabilities,omitempty"`
}

// NoulProb returns the calibrated [0,1] probability for a noul question across both API dialects.
func (a Answer) NoulProb() (float64, bool) {
	var v float64
	switch {
	case a.Noul != nil:
		v = *a.Noul
	case a.Probability != nil:
		v = *a.Probability
	default:
		return 0, false
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v < -1e-6 || v > 1.0+1e-6 {
		return 0, false
	}
	if v < 0 {
		v = 0
	} else if v > 1 {
		v = 1
	}
	return v, true
}

// ScoreValue returns the continuous expected score for a score question.
func (a Answer) ScoreValue() (float64, bool) {
	if a.Score == nil {
		return 0, false
	}
	v := *a.Score
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// ConfidenceValue returns the calibrated [0,1] confidence metric for choice/score questions.
func (a Answer) ConfidenceValue() (float64, bool) {
	if a.Confidence == nil {
		return 0, false
	}
	v := *a.Confidence
	if math.IsNaN(v) || math.IsInf(v, 0) || v < -1e-6 || v > 1.0+1e-6 {
		return 0, false
	}
	if v < 0 {
		v = 0
	} else if v > 1 {
		v = 1
	}
	return v, true
}

// Usage reports token consumption for a System One evaluation call.
type Usage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	Cost         float64 `json:"cost,omitempty"`
}

// Response is the full System One API response.
type Response struct {
	Model     string            `json:"model"`
	RequestID string            `json:"request_id,omitempty"`
	Provider  string            `json:"provider,omitempty"`
	Answers   map[string]Answer `json:"answers"`
	Usage     Usage             `json:"usage"`
}

// Clone returns a deep copy so cached responses cannot be mutated by callers.
func (r *Response) Clone() *Response {
	if r == nil {
		return nil
	}
	cp := *r
	if r.Answers != nil {
		cp.Answers = make(map[string]Answer, len(r.Answers))
		for k, a := range r.Answers {
			ac := a
			if a.Noul != nil {
				v := *a.Noul
				ac.Noul = &v
			}
			if a.Probability != nil {
				v := *a.Probability
				ac.Probability = &v
			}
			if a.Score != nil {
				v := *a.Score
				ac.Score = &v
			}
			if a.Confidence != nil {
				v := *a.Confidence
				ac.Confidence = &v
			}
			if a.Probabilities != nil {
				pm := make(ProbabilityMap, len(a.Probabilities))
				for pk, pv := range a.Probabilities {
					pm[pk] = pv
				}
				ac.Probabilities = pm
			}
			cp.Answers[k] = ac
		}
	}
	return &cp
}

// Evaluator evaluates a System One request against Jev AI (or a test double).
type Evaluator interface {
	Evaluate(ctx context.Context, req Request) (*Response, error)
}

// HealthSnapshot summarizes client runtime health for doctor diagnostics and metrics.
type HealthSnapshot struct {
	Provider            string `json:"provider"`
	Endpoint            string `json:"endpoint"`
	Model               string `json:"model"`
	CircuitOpen         bool   `json:"circuitOpen"`
	ConsecutiveFailures int    `json:"consecutiveFailures"`
	DailyInputTokens    int64  `json:"dailyInputTokens"`
	DailyTokenCap       int64  `json:"dailyTokenCap"`
	CacheEntries        int    `json:"cacheEntries"`
	CacheHits           int64  `json:"cacheHits"`
}

// ValidateResponse strictly verifies that every returned answer matches its declared question type,
// stays within valid numeric bounds, and selects only declared choice options (zero out-of-set drift).
func ValidateResponse(req Request, resp *Response) error {
	if resp == nil {
		return errors.New("jev: empty response")
	}
	if len(resp.Answers) == 0 {
		return errors.New("jev: response contains no answers")
	}
	for qk, q := range req.Questions {
		ans, ok := resp.Answers[qk]
		if !ok {
			return fmt.Errorf("jev: missing answer for question %q", qk)
		}
		if ans.Type != "" && ans.Type != q.Type {
			return fmt.Errorf("jev: question %q type mismatch: want %q got %q", qk, q.Type, ans.Type)
		}
		switch q.Type {
		case TypeNoul:
			if _, ok := ans.NoulProb(); !ok {
				return fmt.Errorf("jev: question %q invalid or missing noul probability", qk)
			}
		case TypeChoice:
			if ans.Choice == "" {
				return fmt.Errorf("jev: question %q empty choice", qk)
			}
			if opts, ok := q.Criteria.(map[string]string); ok && len(opts) > 0 {
				if _, valid := opts[ans.Choice]; !valid {
					return fmt.Errorf("jev: question %q out-of-set choice %q", qk, ans.Choice)
				}
			}
			if ans.Confidence != nil {
				if _, ok := ans.ConfidenceValue(); !ok {
					return fmt.Errorf("jev: question %q invalid confidence", qk)
				}
			}
		case TypeScore:
			sc, ok := ans.ScoreValue()
			if !ok {
				return fmt.Errorf("jev: question %q invalid or missing score", qk)
			}
			if levels, ok := q.Criteria.([]string); ok && len(levels) >= 2 {
				maxLevel := float64(len(levels) - 1)
				if sc < -1e-6 || sc > maxLevel+1e-6 {
					return fmt.Errorf("jev: question %q score %v outside [0, %v]", qk, sc, maxLevel)
				}
			}
			if ans.Confidence != nil {
				if _, ok := ans.ConfidenceValue(); !ok {
					return fmt.Errorf("jev: question %q invalid confidence", qk)
				}
			}
		default:
			return fmt.Errorf("jev: unsupported question type %q", q.Type)
		}
	}
	return nil
}
