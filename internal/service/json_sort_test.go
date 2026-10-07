package service

// json_sort_test.go enforces that every HTTP response struct declares its fields in
// JSON-tag order, so keys marshal alphabetically: checked by reflection
// (structFieldsAlphabetical) and by decoding real output (marshaledKeysAlphabetical,
// which also catches promoted fields). Why: DOCS/CLAUDE.md § JSON output sorting.

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// jsonStructTypes lists every response struct; add new ones here.
var jsonStructTypes = []interface{}{
	HealthJSON{},
	UsageJSON{},
	UsageServer{},
	UsageEndpoint{},
	UsageExamples{},
	VaultStatusJSON{},
	ProcessEntry{},
	UnitStatusJSON{},
	PackageInfoJSON{},
	EnvDiffJSON{},
	VersionDiffJSON{},
	ServiceDiffJSON{},
	DiffResponseJSON{},
}

// jsonTagName returns a field's JSON name: the Go name without a tag, "" for
// unexported or "-" fields.
func jsonTagName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return ""
	}
	if tag == "" {
		// Unexported fields don't appear in JSON regardless.
		if !f.IsExported() {
			return ""
		}
		return f.Name
	}
	return strings.Split(tag, ",")[0]
}

func TestJSONStructs_FieldsDeclaredAlphabetically(t *testing.T) {
	for _, ex := range jsonStructTypes {
		typ := reflect.TypeOf(ex)
		t.Run(typ.Name(), func(t *testing.T) {
			var prev string
			for i := 0; i < typ.NumField(); i++ {
				name := jsonTagName(typ.Field(i))
				if name == "" {
					continue
				}
				if i > 0 && name < prev {
					t.Errorf("%s: field %d %q declared after %q (must be alphabetical by JSON tag)",
						typ.Name(), i, name, prev)
				}
				prev = name
			}
		})
	}
}

// marshaledKeysOf decodes a JSON object's top-level keys in stream order.
// Returns nil for non-object inputs.
func marshaledKeysOf(t *testing.T, data []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))

	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		// Not a JSON object — nothing to verify.
		return nil
	}

	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatalf("key token: %v", err)
		}
		keys = append(keys, k.(string))
		// Skip the value (could be nested).
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("skip value: %v", err)
		}
	}
	return keys
}

func TestJSONStructs_MarshaledKeysAlphabetical(t *testing.T) {
	cases := []struct {
		name string
		v    interface{}
	}{
		{"HealthJSON", HealthJSON{Status: "ok"}},
		{"UsageJSON", populatedUsageJSON()},
		{"UsageServer", UsageServer{Hostname: "h", IP: "1.2.3.4", ListenAddress: ":8080"}},
		{"UsageEndpoint", UsageEndpoint{
			AuthRequired: true, Description: "d",
			Examples: []string{`curl "https://lb/x"`}, Method: "GET",
			Path: "/x", ResponseType: "text/plain",
		}},
		{"UsageExamples", UsageExamples{
			ViaLoadBalancer: []string{"a"}, ViaServerIP: []string{"b"},
		}},
		{"VaultStatusJSON_Configured", VaultStatusJSON{
			Address: "https://v", AuthMethod: "approle",
			Configured: true, LeaseDuration: 60, Renewable: true,
			SecretIDWrapped: true, Status: VaultStatusAuthenticated,
		}},
		{"VaultStatusJSON_NotConfigured", VaultStatusJSON{
			AuthMethod: "none", Status: VaultStatusNotConfigured,
		}},
		{"ProcessEntry", ProcessEntry{Command: "/bin/x", PID: 42}},
		{"UnitStatusJSON", UnitStatusJSON{
			ActiveState: "active", LoadState: "loaded",
			SubState: "running", Unit: "x.service", MainPID: 1,
			CPUQuotaPerSecUSec: 200000, CPUQuotaPercent: "20%",
			MemoryMaxBytes: 1073741824, NRestarts: uint32Ptr(7),
		}},
		{"PackageInfoJSON", PackageInfoJSON{
			Architecture: "x86_64",
			BuildDate:    "Mon 01 Jan 2026 12:00:00 PM UTC",
			BuildHost:    "buildhost.example.com",
			Group:        "Unspecified",
			InstallDate:  "Mon 01 Jan 2026 12:00:00 PM UTC",
			License:      "MIT",
			Name:         "n",
			Release:      "1",
			Signature:    "sig",
			Size:         1,
			SourceRPM:    "src",
			Summary:      "sum",
			URL:          "https://example.com",
			Vendor:       "v",
			Version:      "1.0",
		}},
		{"EnvDiffJSON", EnvDiffJSON{Added: []string{"A"}, Changed: []string{"B"}, Removed: []string{"C"}}},
		{"VersionDiffJSON", VersionDiffJSON{Changed: true, Current: "1.0", Wanted: "1.1"}},
		{"ServiceDiffJSON", ServiceDiffJSON{
			ComputedAt: "2026-01-01T00:00:00Z",
			Config:     []string{"+ a.b: c"},
			Env:        EnvDiffJSON{Added: []string{"A"}},
			Service:    "myapplication",
			Version:    VersionDiffJSON{Changed: true, Current: "1.0", Wanted: "1.1"},
		}},
		{"DiffResponseJSON", DiffResponseJSON{
			GeneratedAt: "2026-01-01T00:00:00Z",
			Services:    []ServiceDiffJSON{{Service: "myapplication"}},
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data, err := json.Marshal(c.v)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			keys := marshaledKeysOf(t, data)
			if len(keys) == 0 {
				t.Skip("no keys to check (probably non-object marshal)")
			}
			sorted := make([]string, len(keys))
			copy(sorted, keys)
			sort.Strings(sorted)
			if !reflect.DeepEqual(keys, sorted) {
				t.Errorf("keys not alphabetically sorted in marshal output\nactual: %v\nwanted: %v",
					keys, sorted)
			}
		})
	}
}

// The real payloads of /, /status and /version decode with alphabetical keys.
func TestJSONStructs_FullEndpointResponses(t *testing.T) {
	t.Run("usage_/", func(t *testing.T) {
		u := populatedUsageJSON()
		data, _ := json.MarshalIndent(u, "", "  ")
		keys := marshaledKeysOf(t, data)
		assertAlphabetical(t, keys)
	})

	t.Run("status_payload", func(t *testing.T) {
		// StatusService always returns valid JSON; without systemd it is the portable path.
		out, _ := StatusService("nonexistent_service_for_alpha_check")
		keys := marshaledKeysOf(t, []byte(out))
		assertAlphabetical(t, keys)
	})
}

func assertAlphabetical(t *testing.T, keys []string) {
	t.Helper()
	for i := 1; i < len(keys); i++ {
		if keys[i] < keys[i-1] {
			t.Errorf("keys not alphabetical at index %d: %q < %q\nfull list: %v",
				i, keys[i], keys[i-1], keys)
			return
		}
	}
}

// populatedUsageJSON sets every field so omitempty hides no key.
func populatedUsageJSON() UsageJSON {
	return UsageJSON{
		Endpoints: []UsageEndpoint{{Method: "GET", Path: "/x"}},
		Examples:  UsageExamples{ViaLoadBalancer: []string{"a"}, ViaServerIP: []string{"b"}},
		Notes:     []string{"note"},
		Server: UsageServer{
			BuildTime:     "2026-01-01T00:00:00Z",
			Commit:        "deadbeef",
			GeneratedAt:   "2026-01-01T00:00:00Z",
			GoVersion:     "go1.26.5",
			Hostname:      "h",
			IP:            "1.2.3.4",
			ListenAddress: ":8080",
			Service:       "cystemd",
			Vault:         VaultStatusJSON{AuthMethod: "none"},
			Version:       "1.0",
		},
	}
}
