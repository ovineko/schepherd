package env

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func setOrUnset(t *testing.T, key string, value *string) {
	t.Helper()
	t.Setenv(key, "")

	if value == nil {
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}

		return
	}

	t.Setenv(key, *value)
}

func TestStringOverrides(t *testing.T) {
	accessors := map[string]func() (string, bool){
		KeyConfig:     Config,
		KeyRepository: Repository,
		KeyCatalog:    Catalog,
		KeyCacheDir:   CacheDir,
		KeyWorkspace:  Workspace,
	}

	cases := []struct {
		value  *string
		name   string
		want   string
		wantOK bool
	}{
		{name: "unset"},
		{name: "empty", value: new("")},
		{name: "blank", value: new(" \t\n")},
		{name: "value", value: new("registry.example/org/schemas"), want: "registry.example/org/schemas", wantOK: true},
		{name: "surrounding spaces are kept", value: new(" a b "), want: " a b ", wantOK: true},
	}

	for key, get := range accessors {
		for _, tc := range cases {
			t.Run(key+"/"+tc.name, func(t *testing.T) {
				setOrUnset(t, key, tc.value)

				if got, ok := get(); got != tc.want || ok != tc.wantOK {
					t.Errorf("%s = %q, %v; want %q, %v", key, got, ok, tc.want, tc.wantOK)
				}
			})
		}
	}
}

func TestOffline(t *testing.T) {
	cases := []struct {
		value   *string
		name    string
		want    bool
		wantSet bool
		wantErr bool
	}{
		{name: "unset"},
		{name: "empty", value: new("")},
		{name: "blank", value: new("  ")},
		{name: "true", value: new("true"), want: true, wantSet: true},
		{name: "1", value: new("1"), want: true, wantSet: true},
		{name: "TRUE", value: new("TRUE"), want: true, wantSet: true},
		{name: "false", value: new("false"), wantSet: true},
		{name: "0", value: new("0"), wantSet: true},
		{name: "yes", value: new("yes"), wantSet: true, wantErr: true},
		{name: "padded", value: new(" true"), wantSet: true, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setOrUnset(t, KeyOffline, tc.value)

			got, set, err := Offline()
			if got != tc.want || set != tc.wantSet || (err != nil) != tc.wantErr {
				t.Fatalf("Offline() = %v, %v, %v; want %v, %v, error %v", got, set, err, tc.want, tc.wantSet, tc.wantErr)
			}

			if err != nil && (!strings.Contains(err.Error(), KeyOffline) || !strings.Contains(err.Error(), "not a boolean")) {
				t.Errorf("error %q does not name the variable and the problem", err)
			}
		})
	}
}

func TestTimeout(t *testing.T) {
	cases := []struct {
		value   *string
		name    string
		want    time.Duration
		wantSet bool
		wantErr bool
	}{
		{name: "unset"},
		{name: "empty", value: new("")},
		{name: "blank", value: new(" ")},
		{name: "seconds", value: new("90s"), want: 90 * time.Second, wantSet: true},
		{name: "compound", value: new("1h2m"), want: time.Hour + 2*time.Minute, wantSet: true},
		{name: "zero", value: new("0s"), wantSet: true, wantErr: true},
		{name: "negative", value: new("-1s"), wantSet: true, wantErr: true},
		{name: "no unit", value: new("30"), wantSet: true, wantErr: true},
		{name: "text", value: new("soon"), wantSet: true, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setOrUnset(t, KeyTimeout, tc.value)

			got, set, err := Timeout()
			if got != tc.want || set != tc.wantSet || (err != nil) != tc.wantErr {
				t.Fatalf("Timeout() = %v, %v, %v; want %v, %v, error %v", got, set, err, tc.want, tc.wantSet, tc.wantErr)
			}

			if err != nil && (!strings.Contains(err.Error(), KeyTimeout) || !strings.Contains(err.Error(), "not a positive duration")) {
				t.Errorf("error %q does not name the variable and the problem", err)
			}
		})
	}
}

func TestLookupAndEnviron(t *testing.T) {
	const key = "SCHEPHERD_ENV_TEST_VALUE"

	setOrUnset(t, key, nil)

	if _, ok := Lookup(key); ok {
		t.Fatal("an unset variable was reported as set")
	}

	t.Setenv(key, "")

	if v, ok := Lookup(key); !ok || v != "" {
		t.Errorf("Lookup of an empty variable = %q, %v; ${NAME} must see it as set", v, ok)
	}

	t.Setenv(key, "a=b")

	if v, ok := Lookup(key); !ok || v != "a=b" {
		t.Errorf("Lookup = %q, %v", v, ok)
	}

	environ := Environ()
	if !slices.Contains(environ, key+"=a=b") {
		t.Errorf("Environ does not contain %s", key)
	}

	environ[0] = "MUTATED=1"
	if slices.Contains(Environ(), "MUTATED=1") {
		t.Error("Environ returned shared state")
	}
}
