package service

// systemd_seam_test.go tests the D-Bus property helpers through the systemdConn
// seam (actions.go), without systemd. It pins two production lessons: per-type
// properties are read on the per-type interface (a .Unit read silently yields
// zero), and a measured zero is not an absent value. Why: DOCS/CLAUDE.md § Testing.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

// fakeSystemdConn embeds systemdConn, so an unexpected call panics (nil method)
// instead of returning a silent zero value.
type fakeSystemdConn struct {
	systemdConn

	unitProps map[string]interface{} // served by GetUnitPropertyContext
	typeProps map[string]interface{} // served by GetUnitTypePropertyContext

	unitErr error
	typeErr error

	// Call recording — how the interface-confusion bug is detected.
	unitCalls []string
	typeCalls []string
	lastType  string
	closed    bool
}

func (f *fakeSystemdConn) Close() { f.closed = true }

func (f *fakeSystemdConn) GetUnitPropertyContext(_ context.Context, _, prop string) (*dbus.Property, error) {
	f.unitCalls = append(f.unitCalls, prop)
	if f.unitErr != nil {
		return nil, f.unitErr
	}
	v, ok := f.unitProps[prop]
	if !ok {
		return nil, errors.New("Unknown interface or property")
	}
	return &dbus.Property{Name: prop, Value: godbus.MakeVariant(v)}, nil
}

func (f *fakeSystemdConn) GetUnitTypePropertyContext(_ context.Context, _, unitType, prop string) (*dbus.Property, error) {
	f.typeCalls = append(f.typeCalls, prop)
	f.lastType = unitType
	if f.typeErr != nil {
		return nil, f.typeErr
	}
	v, ok := f.typeProps[prop]
	if !ok {
		return nil, errors.New("Unknown interface or property")
	}
	return &dbus.Property{Name: prop, Value: godbus.MakeVariant(v)}, nil
}

// TestPropHelpers_ReadValuesOfEachType covers the happy path of every helper.
func TestPropHelpers_ReadValuesOfEachType(t *testing.T) {
	f := &fakeSystemdConn{
		unitProps: map[string]interface{}{
			"ActiveState": "active",
			"SomeUint64":  uint64(42),
			"After":       []string{"a.service", "b.service"},
		},
		typeProps: map[string]interface{}{
			"MainPID":      uint32(1234),
			"Result":       "success",
			"MemoryMax":    uint64(1073741824),
			"NRestarts":    uint32(0),
			"CPUUsageNSec": uint64(999),
		},
	}

	if got := getStringProp(f, "x.service", "ActiveState"); got != "active" {
		t.Errorf("getStringProp = %q, want %q", got, "active")
	}
	if got := getUint64Prop(f, "x.service", "SomeUint64"); got != 42 {
		t.Errorf("getUint64Prop = %d, want 42", got)
	}
	if got := getStringSliceProp(f, "x.service", "After"); len(got) != 2 || got[0] != "a.service" {
		t.Errorf("getStringSliceProp = %v, want [a.service b.service]", got)
	}
	if got := getTypedStringProp(f, "x.service", "Result"); got != "success" {
		t.Errorf("getTypedStringProp = %q, want %q", got, "success")
	}
	if got := getTypedUint32Prop(f, "x.service", "MainPID"); got != 1234 {
		t.Errorf("getTypedUint32Prop = %d, want 1234", got)
	}
	if got := getTypedUint64Prop(f, "x.service", "MemoryMax"); got != 1073741824 {
		t.Errorf("getTypedUint64Prop = %d, want 1073741824", got)
	}
}

// Regression guard for the silently missing /status fields: per-type properties
// go through GetUnitTypePropertyContext on the unit's own interface.
func TestPropHelpers_TypedHelpersUseThePerTypeInterface(t *testing.T) {
	f := &fakeSystemdConn{typeProps: map[string]interface{}{
		"MainPID":   uint32(7),
		"Result":    "success",
		"MemoryMax": uint64(1),
	}}

	getTypedUint32Prop(f, "x.service", "MainPID")
	getTypedStringProp(f, "x.service", "Result")
	getTypedUint64Prop(f, "x.service", "MemoryMax")

	if len(f.unitCalls) != 0 {
		t.Errorf("typed helpers issued .Unit-interface reads %v — per-type properties must never go through GetUnitPropertyContext", f.unitCalls)
	}
	if len(f.typeCalls) != 3 {
		t.Errorf("typed helpers made %d per-type reads, want 3 (%v)", len(f.typeCalls), f.typeCalls)
	}
	if f.lastType != "Service" {
		t.Errorf("per-type read used interface %q, want %q for a .service unit", f.lastType, "Service")
	}
}

// TestPropHelpers_ErrorsDegradeToZeroValues pins the deliberate fail-soft
// behaviour: an unreadable property yields the zero value, never a panic.
func TestPropHelpers_ErrorsDegradeToZeroValues(t *testing.T) {
	f := &fakeSystemdConn{unitErr: errors.New("boom"), typeErr: errors.New("boom")}

	if got := getStringProp(f, "x.service", "ActiveState"); got != "" {
		t.Errorf("getStringProp on error = %q, want empty", got)
	}
	if got := getUint64Prop(f, "x.service", "P"); got != 0 {
		t.Errorf("getUint64Prop on error = %d, want 0", got)
	}
	if got := getStringSliceProp(f, "x.service", "After"); got != nil {
		t.Errorf("getStringSliceProp on error = %v, want nil", got)
	}
	if got := getTypedStringProp(f, "x.service", "Result"); got != "" {
		t.Errorf("getTypedStringProp on error = %q, want empty", got)
	}
	if got := getTypedUint32Prop(f, "x.service", "MainPID"); got != 0 {
		t.Errorf("getTypedUint32Prop on error = %d, want 0", got)
	}
	if got := getTypedUint64Prop(f, "x.service", "MemoryMax"); got != 0 {
		t.Errorf("getTypedUint64Prop on error = %d, want 0", got)
	}
}

// TestPropHelpers_WrongWireTypeDegradesToZero covers the type assertions. A
// property that comes back as an unexpected D-Bus type must not panic.
func TestPropHelpers_WrongWireTypeDegradesToZero(t *testing.T) {
	f := &fakeSystemdConn{
		unitProps: map[string]interface{}{"ActiveState": uint64(9), "SomeUint64": "not-a-number"},
		typeProps: map[string]interface{}{"MainPID": "not-a-uint32", "MemoryMax": "nope", "Result": uint32(3)},
	}
	if got := getStringProp(f, "x.service", "ActiveState"); got != "" {
		t.Errorf("getStringProp on wrong type = %q, want empty", got)
	}
	if got := getUint64Prop(f, "x.service", "SomeUint64"); got != 0 {
		t.Errorf("getUint64Prop on wrong type = %d, want 0", got)
	}
	if got := getTypedUint32Prop(f, "x.service", "MainPID"); got != 0 {
		t.Errorf("getTypedUint32Prop on wrong type = %d, want 0", got)
	}
	if got := getTypedUint64Prop(f, "x.service", "MemoryMax"); got != 0 {
		t.Errorf("getTypedUint64Prop on wrong type = %d, want 0", got)
	}
	if got := getTypedStringProp(f, "x.service", "Result"); got != "" {
		t.Errorf("getTypedStringProp on wrong type = %q, want empty", got)
	}
}

// The zero-vs-absent contract (DESIGN-N-RESTARTS-ZERO-VS-ABSENT.md): a measured 0
// is ok=true, an unreadable property ok=false.
func TestGetTypedUint32PropOK_DistinguishesMeasuredZeroFromAbsent(t *testing.T) {
	measured := &fakeSystemdConn{typeProps: map[string]interface{}{"NRestarts": uint32(0)}}
	if v, ok := getTypedUint32PropOK(measured, "x.service", "NRestarts"); !ok || v != 0 {
		t.Errorf("measured zero: got (%d, %t), want (0, true)", v, ok)
	}

	nonzero := &fakeSystemdConn{typeProps: map[string]interface{}{"NRestarts": uint32(5)}}
	if v, ok := getTypedUint32PropOK(nonzero, "x.service", "NRestarts"); !ok || v != 5 {
		t.Errorf("measured five: got (%d, %t), want (5, true)", v, ok)
	}

	absent := &fakeSystemdConn{typeErr: errors.New("Unknown interface or property")}
	if v, ok := getTypedUint32PropOK(absent, "x.service", "NRestarts"); ok || v != 0 {
		t.Errorf("unreadable: got (%d, %t), want (0, false)", v, ok)
	}

	// A property present but of the wrong wire type is also "cannot tell".
	wrongType := &fakeSystemdConn{typeProps: map[string]interface{}{"NRestarts": "3"}}
	if v, ok := getTypedUint32PropOK(wrongType, "x.service", "NRestarts"); ok || v != 0 {
		t.Errorf("wrong wire type: got (%d, %t), want (0, false)", v, ok)
	}
}

// TestGetStringSliceProp_AcceptsBothWireShapes covers the []interface{} arm,
// which is what godbus actually hands back for some array properties.
func TestGetStringSliceProp_AcceptsBothWireShapes(t *testing.T) {
	native := &fakeSystemdConn{unitProps: map[string]interface{}{"After": []string{"a", "b"}}}
	if got := getStringSliceProp(native, "x.service", "After"); len(got) != 2 || got[1] != "b" {
		t.Errorf("[]string shape = %v, want [a b]", got)
	}

	// Mixed slice: non-strings are dropped rather than stringified or panicked on.
	boxed := &fakeSystemdConn{unitProps: map[string]interface{}{"After": []interface{}{"a", uint32(7), "b"}}}
	got := getStringSliceProp(boxed, "x.service", "After")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("[]interface{} shape = %v, want [a b] with the non-string dropped", got)
	}
}

// stubSystemdConn swaps openSystemdConn for the duration of a test, restoring
// it via t.Cleanup. Mirrors stubUnitHealth's shape in unit_metrics_test.go.
func stubSystemdConn(t *testing.T, conn systemdConn, err error) {
	t.Helper()
	prev := openSystemdConn
	openSystemdConn = func() (systemdConn, error) { return conn, err }
	t.Cleanup(func() { openSystemdConn = prev })
}

// StatusService end to end without systemd.
func TestStatusService_BuildsStatusFromProperties(t *testing.T) {
	f := &fakeSystemdConn{
		unitProps: map[string]interface{}{
			"ActiveState":    "active",
			"SubState":       "running",
			"Description":    "Demo service",
			"LoadState":      "loaded",
			"FragmentPath":   "/etc/systemd/system/demo.service",
			"UnitFileState":  "enabled",
			"UnitFilePreset": "disabled",
			"DropInPaths":    []string{"/etc/systemd/system.control/50-CPUQuota.conf"},
		},
		typeProps: map[string]interface{}{
			"MemoryCurrent": uint64(1000),
			"MemoryMax":     uint64(2000),
			"ControlGroup":  "/system.slice/demo.service",
			"NRestarts":     uint32(0),
		},
	}
	stubSystemdConn(t, f, nil)

	out, err := StatusService("demo")
	if err != nil {
		t.Fatalf("StatusService: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("StatusService returned invalid JSON: %v\n%s", err, out)
	}
	for k, want := range map[string]string{
		"active_state": "active",
		"sub_state":    "running",
		"description":  "Demo service",
		"load_state":   "loaded",
		"unit":         "demo.service",
		"cgroup":       "/system.slice/demo.service",
	} {
		if got[k] != want {
			t.Errorf("%s = %v, want %q", k, got[k], want)
		}
	}

	if !f.closed {
		t.Error("StatusService did not close the D-Bus connection")
	}
}

// JSON half of zero-vs-absent: a never-restarted unit reports n_restarts: 0
// (*uint32, so omitempty keeps a real zero).
func TestStatusService_MeasuredZeroRestartsIsEmitted(t *testing.T) {
	f := &fakeSystemdConn{
		unitProps: map[string]interface{}{"ActiveState": "active"},
		typeProps: map[string]interface{}{"NRestarts": uint32(0)},
	}
	stubSystemdConn(t, f, nil)

	out, err := StatusService("demo")
	if err != nil {
		t.Fatalf("StatusService: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	v, present := got["n_restarts"]
	if !present {
		t.Fatalf("n_restarts was omitted for a measured zero; got: %s", out)
	}
	if v != float64(0) {
		t.Errorf("n_restarts = %v, want 0", v)
	}
}

// TestStatusService_AbsentRestartsIsOmitted is the other half: a unit type that
// carries no NRestarts at all must omit the field rather than report a fake 0.
func TestStatusService_AbsentRestartsIsOmitted(t *testing.T) {
	f := &fakeSystemdConn{
		unitProps: map[string]interface{}{"ActiveState": "active"},
		typeProps: map[string]interface{}{}, // NRestarts unreadable
	}
	stubSystemdConn(t, f, nil)

	out, err := StatusService("demo")
	if err != nil {
		t.Fatalf("StatusService: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, present := got["n_restarts"]; present {
		t.Errorf("n_restarts was emitted for an unreadable property; want it omitted. got: %s", out)
	}
}

// Unreachable systemd: valid JSON naming the unit and the error, never a Go error.
func TestStatusService_ConnectFailureReturnsStructuredJSON(t *testing.T) {
	stubSystemdConn(t, nil, errors.New("dial refused"))

	out, err := StatusService("demo")
	if err != nil {
		t.Fatalf("StatusService should degrade, not error: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("degraded output is not valid JSON: %v\n%s", err, out)
	}
	if got["unit"] != "demo.service" {
		t.Errorf("unit = %q, want demo.service", got["unit"])
	}
	if !strings.Contains(got["error"], "dial refused") {
		t.Errorf("error = %q, want it to name the underlying failure", got["error"])
	}
}

// The real collector behind the unit-health metrics (unit_metrics_test.go stubs it).
func TestCollectUnitHealthFromSystemd_SamplesEveryManagedUnit(t *testing.T) {
	f := &fakeSystemdConn{
		unitProps: map[string]interface{}{"ActiveState": "active"},
		typeProps: map[string]interface{}{"NRestarts": uint32(3)},
	}
	stubSystemdConn(t, f, nil)

	snap := collectUnitHealthFromSystemd([]string{"alpha", "beta"})
	if !snap.SystemdOK {
		t.Fatal("SystemdOK = false, want true")
	}
	if len(snap.Units) != 2 {
		t.Fatalf("sampled %d units, want 2", len(snap.Units))
	}
	for _, u := range snap.Units {
		if u.ActiveState != "active" {
			t.Errorf("%s: ActiveState = %q, want active", u.Service, u.ActiveState)
		}
		if !u.NRestartsOK || u.NRestarts != 3 {
			t.Errorf("%s: NRestarts = (%d, %t), want (3, true)", u.Service, u.NRestarts, u.NRestartsOK)
		}
	}
}

// A wedged D-Bus reports SystemdOK=false with no units, never "every unit is down".
func TestCollectUnitHealthFromSystemd_ConnectFailureIsNotSilentlyHealthy(t *testing.T) {
	stubSystemdConn(t, nil, errors.New("dial refused"))

	snap := collectUnitHealthFromSystemd([]string{"alpha"})
	if snap.SystemdOK {
		t.Error("SystemdOK = true after a connect failure")
	}
	if len(snap.Units) != 0 {
		t.Errorf("got %d units after a connect failure, want 0 (absent, not zero)", len(snap.Units))
	}
}
