package cookiejar

import "testing"

// TestIsEssentialKeepsTheCredentials pins the cookies Flow genuinely depends on.
// Dropping one of these breaks the session exchange in a way that shows up much
// later, as an unexplained 401.
func TestIsEssentialKeepsTheCredentials(t *testing.T) {
	for _, name := range []string{
		"__Secure-next-auth.session-token",
		"__Secure-next-auth.callback-url",
		"__Host-next-auth.csrf-token",
		"SID", "HSID", "SSID", "APISID", "SAPISID",
		"__Secure-1PSID", "__Secure-3PSID",
		"__Secure-1PSIDTS", "__Secure-3PSIDTS",
		"EMAIL", "OSID", "__Secure-OSID",
	} {
		if !IsEssential(name) {
			t.Errorf("%s should be kept", name)
		}
	}
}

// TestIsEssentialDropsTheNoise is the point of the filter.
//
// A signed-in profile carries hundreds of these — analytics for every Google
// property, and a separate session for Mail, Drive, Play, NotebookLM and Colab.
// None of them have any bearing on Flow, and together they made a 19 kB Cookie
// header.
func TestIsEssentialDropsTheNoise(t *testing.T) {
	for _, name := range []string{
		"_ga", "_ga_X2GNH8R5NS", "_gcl_au", "_gcl_aw", "_gcl_gs", "__utmz",
		"COMPASS", "OTZ", "ext_name", "AEC", "NID",
		"__Secure-1PSIDCC", "__Secure-3PSIDCC",
	} {
		if IsEssential(name) {
			t.Errorf("%s is not needed and should be dropped", name)
		}
	}
}

// TestIsEssentialIsCaseInsensitive keeps the filter aligned with the rest of the
// package, which compares cookie names case-insensitively throughout.
func TestIsEssentialIsCaseInsensitive(t *testing.T) {
	if !IsEssential("sid") || !IsEssential("Sid") {
		t.Error("cookie names should compare case-insensitively")
	}
}

// TestEssentialCoversAuthCookies guards the relationship between the two lists.
// Everything that makes a jar usable must also survive the sync filter, or the
// engine would accept a jar it had just thrown the credentials out of.
func TestEssentialCoversAuthCookies(t *testing.T) {
	for _, name := range AuthCookieNames {
		if !IsEssential(name) {
			t.Errorf("%s decides a jar is usable but would be filtered out on sync", name)
		}
	}
}
