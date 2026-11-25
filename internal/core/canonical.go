package core

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
)

func marshalJSONNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b, nil
}

// normalizeJSONNumber formats a JSON number according to RFC 8785 (JCS) rules:
// rejects NaN/Inf, normalizes -0 to 0, renders exact integer floats (e.g. 1.0, 1e2)
// without fractional zeroes, and preserves arbitrary-precision integers.
func normalizeJSONNumber(n json.Number) (string, error) {
	s := strings.TrimSpace(n.String())
	if s == "" {
		return "", errors.New("canonical json: empty number")
	}
	if !strings.ContainsAny(s, ".eE") {
		if iv, err := strconv.ParseInt(s, 10, 64); err == nil {
			if iv == 0 {
				return "0", nil
			}
			return strconv.FormatInt(iv, 10), nil
		}
		if uv, err := strconv.ParseUint(s, 10, 64); err == nil {
			return strconv.FormatUint(uv, 10), nil
		}
		return s, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return "", err
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", errors.New("canonical json: non-finite float")
	}
	if f == 0 {
		return "0", nil
	}
	if math.Trunc(f) == f && math.Abs(f) < 1e21 {
		return strconv.FormatFloat(f, 'f', 0, 64), nil
	}
	return strconv.FormatFloat(f, 'g', -1, 64), nil
}

// CanonicalJSON serializes any JSON-compatible value deterministically (RFC 8785 style):
// recursively sorted object keys, no HTML escaping of <, >, &, normalized numbers, compact separators.
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := marshalJSONNoEscape(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, generic); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := marshalJSONNoEscape(k)
			if err != nil {
				return err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case json.Number:
		ns, err := normalizeJSONNumber(x)
		if err != nil {
			return err
		}
		buf.WriteString(ns)
	default:
		b, err := marshalJSONNoEscape(x)
		if err != nil {
			return err
		}
		buf.Write(b)
	}
	return nil
}

// HashOf computes hex(DSHA256(CanonicalJSON(v))).
func HashOf(v any) (string, error) {
	b, err := CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(DSHA256(b)), nil
}
