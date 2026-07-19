package config

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzLoad drives arbitrary bytes through Load() by way of a real temp file
// on disk — the same path a corrupted or hand-edited config.json would take
// in production. The only contract is: never panic. A parse or validation
// error is a perfectly fine, expected outcome for garbage input.
func FuzzLoad(f *testing.F) {
	seeds := []string{
		toolkitShape,
		`{}`,
		`{"Providers":[]}`,
		`{"Providers":[{"name":"a"}]}`,
		`{"Providers":[{"name":"a","api_base_url":"https://a/b"}]}`,
		`{"Providers":[{"name":"a","api_base_url":"ftp://a/b"}]}`,
		`{"Providers":[{"name":"a","api_base_url":"https://a/b"},{"name":"a","api_base_url":"https://c/d"}]}`,
		`{"Router":{"default":"ghost,m"}}`,
		`{"Router":{"default":"no-comma"}}`,
		`[]`,
		`null`,
		`42`,
		`"just a string"`,
		`{"Providers": [`,
		``,
		`{`,
		`{"Providers":[{"name":"a","api_base_url":"https://a/b","transformer":{"use":["cleancache"]}}]}`,
		`{"Providers":[{"name":123}]}`,
		`{"Providers":"not-an-array"}`,
		`{"Providers":[{"name":"a","api_base_url":"https://a/b","models":[1,2,3]}]}`,
		"\x00\x01\x02binary garbage\xff\xfe",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Load panicked on input %q: %v", data, r)
			}
		}()

		// A fresh file per call, inside the fuzz corpus's own temp dir, so
		// concurrent fuzz workers (go test -fuzz runs them in parallel) never
		// collide on the same path.
		p := filepath.Join(dir, "fuzz-config.json")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatalf("write fuzz input to disk: %v", err)
		}

		c, err := Load(p)
		if err != nil {
			return // parse/validate errors are an expected, non-panicking outcome
		}
		if c == nil {
			t.Fatalf("Load returned nil config with nil error for input %q", data)
		}
		// A config that claims to have loaded successfully must itself
		// re-validate cleanly — Load already calls Validate, but confirming
		// it here guards against any future refactor that forgets to.
		if err := c.Validate(); err != nil {
			t.Fatalf("Load succeeded but the resulting config fails Validate(): %v (input %q)", err, data)
		}
	})
}
