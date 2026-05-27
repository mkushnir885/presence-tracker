package i18n

import "testing"

func TestCatalogLookupAndFallback(t *testing.T) {
	c := New()
	if err := c.Add("en", []byte(`{"greet":"Hello","ask":"Who?"}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.Add("uk", []byte(`{"greet":"Вітаю"}`)); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		lang string
		key  string
		want string
	}{
		{"hit primary", "en", "greet", "Hello"},
		{"hit secondary", "uk", "greet", "Вітаю"},
		{"missing key falls back to key", "en", "nope", "nope"},
		{"missing lang falls back to key", "fr", "greet", "greet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.Locale(tc.lang).T(tc.key)
			if got != tc.want {
				t.Errorf("T(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

func TestCatalogAddMerges(t *testing.T) {
	c := New()
	_ = c.Add("en", []byte(`{"a":"1","b":"2"}`))
	_ = c.Add("en", []byte(`{"b":"override","c":"3"}`))

	loc := c.Locale("en")
	for k, want := range map[string]string{"a": "1", "b": "override", "c": "3"} {
		if got := loc.T(k); got != want {
			t.Errorf("T(%q) = %q, want %q", k, got, want)
		}
	}
}

func TestCatalogAddInvalidJSON(t *testing.T) {
	c := New()
	if err := c.Add("en", []byte(`not json`)); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestZeroLocaleReturnsKey(t *testing.T) {
	var l Locale
	if got := l.T("anything"); got != "anything" {
		t.Errorf("zero Locale T = %q, want %q", got, "anything")
	}
}
