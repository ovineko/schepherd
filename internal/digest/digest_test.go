package digest

import (
	"errors"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	good := FromBytes([]byte("{}"))
	if err := Validate(good); err != nil {
		t.Fatalf("Validate(%q): %v", good, err)
	}

	if got, want := good, "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"; got != want {
		t.Errorf("FromBytes({}) = %s, want %s", got, want)
	}

	invalid := []string{
		"",
		"sha256",
		"sha256:",
		":abc",
		strings.ToUpper(good),
		"sha256:" + strings.Repeat("A", 64),
		"sha256:" + strings.Repeat("a", 63),
		"sha256:" + strings.Repeat("a", 65),
		"sha256:" + strings.Repeat("g", 64),
		"SHA256:" + strings.Repeat("a", 64),
		"sha 256:" + strings.Repeat("a", 64),
		"-x:" + strings.Repeat("a", 64),
	}

	for _, s := range invalid {
		if err := Validate(s); !errors.Is(err, ErrInvalid) {
			t.Errorf("Validate(%q) = %v, want ErrInvalid", s, err)
		}
	}

	for _, s := range []string{"sha512:" + strings.Repeat("a", 128), "sha512:00", "sha512:!!", "blake3:" + strings.Repeat("a", 64)} {
		if err := Validate(s); !errors.Is(err, ErrUnsupportedAlgorithm) {
			t.Errorf("Validate(%q) = %v, want ErrUnsupportedAlgorithm", s, err)
		}
	}
}

func FuzzValidate(f *testing.F) {
	f.Add("sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a")
	f.Add("sha512:00")
	f.Add("")

	f.Fuzz(func(t *testing.T, s string) {
		if Validate(s) == nil && (len(s) != len(prefix)+64 || !strings.HasPrefix(s, prefix)) {
			t.Fatalf("accepted %q", s)
		}
	})
}
