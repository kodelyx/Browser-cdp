package cdp

import (
	"encoding/json"
	"testing"
)

// TestDecodeFlowCaptchaAcceptsEveryShape pins the three answers this has to
// understand.
//
// The two extension paths differ, and getting this wrong is silent: a probe that
// fails to decode reads as "the page has no reCAPTCHA client", which drops the
// broker to the low-score HTTP provider rather than raising anything. The only
// symptom is a worse captcha, so the shape handling is asserted rather than
// assumed.
func TestDecodeFlowCaptchaAcceptsEveryShape(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		available bool
		token     string
	}{
		{"object from the Flow extension", `{"available":true,"token":"abc"}`, true, "abc"},
		{"object probe", `{"available":false}`, false, ""},
		{"bare boolean true, from the probe fallback", `true`, true, ""},
		{"bare boolean false, from the probe fallback", `false`, false, ""},
		{"bare token, from an older mint fallback", `"token-value"`, true, "token-value"},
	}

	for _, tc := range cases {
		got, err := decodeFlowCaptcha(json.RawMessage(tc.raw))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got.Available != tc.available {
			t.Errorf("%s: available = %v, want %v", tc.name, got.Available, tc.available)
		}
		if got.Token != tc.token {
			t.Errorf("%s: token = %q, want %q", tc.name, got.Token, tc.token)
		}
	}
}

// TestDecodeFlowCaptchaRejectsWhatItCannotRead keeps a genuinely unexpected
// payload loud. Silently returning an empty result is what made the original
// shape mismatch invisible.
func TestDecodeFlowCaptchaRejectsWhatItCannotRead(t *testing.T) {
	for _, raw := range []string{`123`, `[]`, `null`, `{}`} {
		if _, err := decodeFlowCaptcha(json.RawMessage(raw)); err == nil {
			t.Errorf("%s should not decode as a captcha answer", raw)
		}
	}
}

// TestTruncateRawKeepsLogsShort covers the helper that reports an unreadable
// payload without dumping a whole page into the log.
func TestTruncateRawKeepsLogsShort(t *testing.T) {
	short := json.RawMessage(`{"a":1}`)
	if got := truncateRaw(short); got != string(short) {
		t.Errorf("a short payload should pass through, got %q", got)
	}

	long := make([]byte, 500)
	for i := range long {
		long[i] = 'x'
	}
	got := truncateRaw(long)
	if len(got) > 130 {
		t.Errorf("a long payload should be truncated, got %d chars", len(got))
	}
	if got[len(got)-3:] != "..." {
		t.Errorf("a truncated payload should say so, got %q", got[len(got)-10:])
	}
}
