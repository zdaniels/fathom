package brandenv

import "testing"

func TestGetCompatibility(t *testing.T) {
	t.Setenv("FANTAZM_TEST_SETTING", "legacy")
	if got := Get("FATHOM_TEST_SETTING"); got != "legacy" {
		t.Fatalf("legacy fallback: %q", got)
	}
	t.Setenv("FATHOM_TEST_SETTING", "current")
	if got := Get("FATHOM_TEST_SETTING"); got != "current" {
		t.Fatalf("current precedence: %q", got)
	}
	t.Setenv("FATHOM_TEST_SETTING", "")
	if got := Get("FATHOM_TEST_SETTING"); got != "" {
		t.Fatalf("explicit empty override: %q", got)
	}
}
