package release

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseAndCompareVersions(t *testing.T) {
	ordered := []string{"0.1.0-alpha", "0.1.0-alpha.1", "0.1.0-alpha.beta", "0.1.0-beta", "0.1.0-beta.2",
		"0.1.0-beta.11", "0.1.0-rc.1", "0.1.0", "0.1.1", "0.2.0", "1.0.0", "v10.0.0"}
	for i := range ordered {
		a, err := ParseVersion(ordered[i])
		if err != nil {
			t.Fatal(err)
		}
		if same, _ := ParseVersion(ordered[i]); a.Compare(same) != 0 {
			t.Errorf("%s != itself", a)
		}
		for j := i + 1; j < len(ordered); j++ {
			b, _ := ParseVersion(ordered[j])
			if a.Compare(b) != -1 || b.Compare(a) != 1 {
				t.Errorf("%s isn't older than %s", a, b)
			}
		}
	}
	v, _ := ParseVersion("v0.2.0-rc.1")
	if v.String() != "0.2.0-rc.1" || v.Tag() != "v0.2.0-rc.1" || !v.IsPre() {
		t.Errorf("v = %+v", v)
	}
	for _, bad := range []string{"", "0.1", "0.1.0-RC1", "01.1.0", "0.1.0-", "latest", "0.1.0 ", "0.1.0/../x"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q): no error", bad)
		}
	}
}

func TestReplaceBinary(t *testing.T) {
	src := filepath.Join(t.TempDir(), "parcon")
	if err := os.WriteFile(src, []byte("new parcon"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "parcon")
	if err := os.WriteFile(dst, []byte("old parcon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceBinary(src, dst); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(dst)
	if data, _ := os.ReadFile(dst); string(data) != "new parcon" || st.Mode().Perm() != 0o755 {
		t.Errorf("dst = %q, %v", data, st.Mode())
	}
}
