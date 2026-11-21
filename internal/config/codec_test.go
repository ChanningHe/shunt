package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	yaml "go.yaml.in/yaml/v3"
)

var allExts = []string{"toml", "yaml", "yml", "json"}

// render writes a format-neutral tree with the raw format library, so the
// test input does not depend on the codec under test.
func render(t *testing.T, ext string, tree map[string]any) []byte {
	t.Helper()
	var (
		b   []byte
		err error
	)
	switch formatOf(ext) {
	case "toml":
		b, err = toml.Marshal(tree)
	case "yaml":
		b, err = yaml.Marshal(tree)
	case "json":
		b, err = json.Marshal(tree)
	}
	if err != nil {
		t.Fatalf("render %s: %v", ext, err)
	}
	return b
}

type tree = map[string]any

func TestDecodeConfigAllFormats(t *testing.T) {
	full := tree{
		"listen": "127.0.0.1:9000",
		"providers": tree{
			"deepseek": tree{
				"base_url":    "https://api.deepseek.com/anthropic",
				"beta_allow":  []any{"b1"},
				"top_keys":    []any{"model", "messages"},
				"block_types": []any{"text"},
			},
		},
		"routes": tree{
			"sonnet": tree{"provider": "deepseek", "model": "deepseek-flash"},
		},
	}
	fullWant := Config{
		Listen: "127.0.0.1:9000",
		Providers: map[string]Provider{"deepseek": {
			BaseURL:    "https://api.deepseek.com/anthropic",
			BetaAllow:  []string{"b1"},
			TopKeys:    []string{"model", "messages"},
			BlockTypes: []string{"text"},
		}},
		Routes: map[string]Route{"sonnet": {Provider: "deepseek", Model: "deepseek-flash"}},
	}
	cases := []struct {
		name string
		in   tree
		want *Config // nil = must be rejected
	}{
		{"empty", tree{}, &Config{}},
		{"full", full, &fullWant},
		{"top-level anthropic", tree{"anthropic": "https://evil.example"}, nil},
		{"provider key", tree{"providers": tree{"d": tree{"base_url": "https://x.example", "key": "sk-1"}}}, nil},
		{"provider key_env", tree{"providers": tree{"d": tree{"base_url": "https://x.example", "key_env": "K"}}}, nil},
		{"unknown route field", tree{"routes": tree{"r": tree{"provider": "d", "model": "m", "prefix": "x/"}}}, nil},
		{"listen is a number", tree{"listen": 5}, nil},
		{"providers is a list", tree{"providers": []any{"a"}}, nil},
		{"top_keys is a string", tree{"providers": tree{"d": tree{"base_url": "https://x.example", "top_keys": "model"}}}, nil},
	}
	for _, tc := range cases {
		for _, ext := range allExts {
			t.Run(tc.name+"/"+ext, func(t *testing.T) {
				var got Config
				err := decode(formatOf(ext), render(t, ext, tc.in), &got)
				if tc.want == nil {
					if err == nil {
						t.Fatalf("accepted: %+v", got)
					}
					return
				}
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				if !reflect.DeepEqual(got, *tc.want) {
					t.Fatalf("got %+v, want %+v", got, *tc.want)
				}
			})
		}
	}
}

func TestDecodeKeysAllFormats(t *testing.T) {
	cases := []struct {
		name string
		in   tree
		want Keys // nil = must be rejected
	}{
		{"literal and file", tree{"a": "sk-1", "b": "file:///run/secrets/b"}, Keys{"a": "sk-1", "b": "file:///run/secrets/b"}},
		{"number value", tree{"a": 1}, nil},
		{"nested value", tree{"a": tree{"key": "sk-1"}}, nil},
	}
	for _, tc := range cases {
		for _, ext := range allExts {
			t.Run(tc.name+"/"+ext, func(t *testing.T) {
				var got Keys
				err := decode(formatOf(ext), render(t, ext, tc.in), &got)
				if tc.want == nil {
					if err == nil {
						t.Fatalf("accepted: %v", got)
					}
					return
				}
				if err != nil || !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("got %v, %v", got, err)
				}
			})
		}
	}
}

func TestDecodeSyntaxErrorsAndEmpty(t *testing.T) {
	broken := map[string]string{
		"toml": "listen = \n",
		"yaml": "listen: [\n",
		"yml":  "listen: [\n",
		"json": "{\"listen\": ",
	}
	for _, ext := range allExts {
		var c Config
		if err := decode(formatOf(ext), []byte(broken[ext]), &c); err == nil {
			t.Errorf("%s: syntax error accepted", ext)
		}
		if err := decode(formatOf(ext), []byte(" \n"), &c); err != nil || !reflect.DeepEqual(c, Config{}) {
			t.Errorf("%s: empty file: %+v %v", ext, c, err)
		}
		if err := decode(formatOf(ext), []byte("[1, 2]"), &c); err == nil {
			t.Errorf("%s: top-level list accepted", ext)
		}
	}
}

func TestEncodeRoundTripStable(t *testing.T) {
	cfgs := []Config{
		{},
		validConfig(),
		{
			Providers: map[string]Provider{
				"b": {BaseURL: "https://b.example", BetaAllow: []string{"x", "y"}, TopKeys: []string{"model"}},
				"a": {BaseURL: "http://127.0.0.1:1/p"},
			},
			Routes: map[string]Route{
				"z":     {Provider: "a", Model: "m-1"},
				"haiku": {Provider: "b", Model: "m-2"},
			},
		},
	}
	for _, ext := range allExts {
		for i, c := range cfgs {
			first, err := encode(formatOf(ext), c)
			if err != nil {
				t.Fatalf("%s/%d encode: %v", ext, i, err)
			}
			var back Config
			if err := decode(formatOf(ext), first, &back); err != nil {
				t.Fatalf("%s/%d decode: %v\n%s", ext, i, err, first)
			}
			if !reflect.DeepEqual(back, c) {
				t.Fatalf("%s/%d: round trip changed value: %+v vs %+v", ext, i, back, c)
			}
			second, err := encode(formatOf(ext), back)
			if err != nil || string(first) != string(second) {
				t.Fatalf("%s/%d: re-encode not byte stable:\n%s\n---\n%s", ext, i, first, second)
			}
		}
		keys := Keys{"b": "file:///k", "a": "sk-1"}
		kb, err := encode(formatOf(ext), keys)
		if err != nil {
			t.Fatal(err)
		}
		var kback Keys
		if err := decode(formatOf(ext), kb, &kback); err != nil || !reflect.DeepEqual(kback, keys) {
			t.Fatalf("%s keys round trip: %v %v", ext, kback, err)
		}
	}
}

func TestEncodeDoesNotMaterializeDefaults(t *testing.T) {
	c := Config{
		Providers: map[string]Provider{"d": {BaseURL: "https://x.example", TopKeys: []string{}}},
		Routes:    map[string]Route{"r": {Provider: "d", Model: "m"}},
	}
	for _, ext := range allExts {
		b, err := encode(formatOf(ext), c)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"listen", "top_keys", "block_types", "beta_allow"} {
			if strings.Contains(string(b), field) {
				t.Errorf("%s: %q materialized:\n%s", ext, field, b)
			}
		}
	}
}
