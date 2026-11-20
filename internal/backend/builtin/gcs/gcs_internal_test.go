package gcs

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode"
)

// TestFoldNameMatchesEncodingJSON checks foldName against encoding/json
// itself: for many spellings of a key's field names, encoding/json sets
// the field from a member exactly when the member's name folds like the
// field's. So checkKey's duplicate check sees every pair of names that
// could set the same field, in Syncr as in rclone, and only those. For
// token_uri, every spelling encoding/json takes for it is refused: alone
// with another address, and next to the real one.
func TestFoldNameMatchesEncodingJSON(t *testing.T) {
	t.Parallel()
	var good map[string]any
	if err := json.Unmarshal([]byte(exampleKey("", false)), &good); err != nil {
		t.Fatal(err)
	}
	keyWith := func(drop, name, value string) string {
		m := map[string]any{}
		for k, v := range good {
			if k != drop {
				m[k] = v
			}
		}
		m[name] = value
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	matched := 0
	for _, field := range []string{"type", "client_email", "private_key", "token_uri", "universe_domain"} {
		typ := reflect.StructOf([]reflect.StructField{{
			Name: "V", Type: reflect.TypeFor[string](), Tag: reflect.StructTag(`json:"` + field + `"`),
		}})
		for _, name := range spellings(field) {
			doc, err := json.Marshal(map[string]string{name: "X"})
			if err != nil {
				t.Fatal(err)
			}
			v := reflect.New(typ)
			if err := json.Unmarshal(doc, v.Interface()); err != nil {
				t.Fatal(err)
			}
			sets := v.Elem().Field(0).String() == "X"
			if folds := foldName(name) == foldName(field); folds != sets {
				t.Errorf("%q for %q: encoding/json sets the field: %v; foldName matches: %v", name, field, sets, folds)
			}
			if !sets || field != "token_uri" {
				continue
			}
			matched++
			if msg := checkKey(keyWith("token_uri", name, "http://127.0.0.1:9/token")); msg == "" {
				t.Errorf("a key whose token_uri is spelled %q and names another address is accepted", name)
			}
			if name != field {
				if msg := checkKey(keyWith("", name, "http://127.0.0.1:9/token")); msg != "names a field twice" {
					t.Errorf("a key with token_uri and %q: %q, want a repeated name", name, msg)
				}
			}
		}
	}
	if matched < 4 {
		t.Errorf("only %d spellings of token_uri matched; the test lost its cases", matched)
	}
}

// spellings returns name in other cases and with look-alike runes: all in
// upper case, title case, each rune swapped for the other runes of its
// case folding orbit (the Kelvin sign for "k", the long s for "s"), and
// swaps encoding/json does not match: a dotted capital or dotless small i
// for "i", a dash, a full-width low line or nothing for "_".
func spellings(name string) []string {
	out := []string{name, strings.ToUpper(name), strings.ToUpper(name[:1]) + name[1:]}
	runes := []rune(name)
	swap := func(i int, r rune) {
		s := append([]rune{}, runes...)
		s[i] = r
		out = append(out, string(s))
	}
	for i, r := range runes {
		for o := unicode.SimpleFold(r); o != r; o = unicode.SimpleFold(o) {
			swap(i, o)
		}
		switch r {
		case 'i':
			swap(i, 'İ')
			swap(i, 'ı')
		case '_':
			swap(i, '-')
			swap(i, '＿')
			out = append(out, string(runes[:i])+string(runes[i+1:]))
		}
	}
	return out
}
