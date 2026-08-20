package discovery

import (
	"strings"
	"testing"
)

func TestParseForwardSinglePort(t *testing.T) {
	maps, err := ParseForward("5432")
	if err != nil {
		t.Fatalf("ParseForward: %v", err)
	}
	if len(maps) != 1 {
		t.Fatalf("maps = %+v, want one", maps)
	}
	pm := maps[0]
	if pm.Src() != SourceStatic {
		t.Errorf("Source = %q, want static", pm.Src())
	}
	if pm.Target() != "127.0.0.1:5432" {
		t.Errorf("Target() = %q, want the remote loopback", pm.Target())
	}
	if pm.PreferredLocal() != 5432 {
		t.Errorf("PreferredLocal() = %d, want the same number locally", pm.PreferredLocal())
	}
	// Published keeps the row out of the "unpublished, via container IP" note:
	// there is a real address on the remote side to dial.
	if !pm.Published() || !pm.Forwardable() || pm.Offered {
		t.Errorf("row = %+v, want a published, forwardable, enabled row", pm)
	}
	if pm.Name != "forward 5432" {
		t.Errorf("Name = %q, want a name derived from the port", pm.Name)
	}
}

func TestParseForwardLocalAndRemoteDiffer(t *testing.T) {
	maps, err := ParseForward("15432:5432")
	if err != nil {
		t.Fatalf("ParseForward: %v", err)
	}
	if got := maps[0].PreferredLocal(); got != 15432 {
		t.Errorf("PreferredLocal() = %d, want 15432", got)
	}
	if got := maps[0].Target(); got != "127.0.0.1:5432" {
		t.Errorf("Target() = %q, want the remote port", got)
	}
}

func TestParseForwardRange(t *testing.T) {
	maps, err := ParseForward("8000-8010")
	if err != nil {
		t.Fatalf("ParseForward: %v", err)
	}
	if len(maps) != 11 {
		t.Fatalf("maps has %d entries, want 11", len(maps))
	}
	keys := map[string]bool{}
	for i, pm := range maps {
		if want := 8000 + i; pm.ContainerPort != want || pm.PreferredLocal() != want {
			t.Errorf("entry %d = %d -> %d, want %d one for one", i, pm.PreferredLocal(), pm.ContainerPort, want)
		}
		if keys[pm.Key()] {
			t.Fatalf("entry %d reuses key %q; the rows would collapse into one", i, pm.Key())
		}
		keys[pm.Key()] = true
	}
}

func TestParseForwardOffsetRange(t *testing.T) {
	maps, err := ParseForward("15432-15434:8000-8002")
	if err != nil {
		t.Fatalf("ParseForward: %v", err)
	}
	if len(maps) != 3 {
		t.Fatalf("maps has %d entries, want 3", len(maps))
	}
	if maps[2].PreferredLocal() != 15434 || maps[2].ContainerPort != 8002 {
		t.Errorf("last entry = %d -> %d, want 15434 -> 8002", maps[2].PreferredLocal(), maps[2].ContainerPort)
	}
}

func TestParseForwardLabelAndRemoteHost(t *testing.T) {
	maps, err := ParseForward("db=5432@10.0.0.5")
	if err != nil {
		t.Fatalf("ParseForward: %v", err)
	}
	pm := maps[0]
	if pm.Name != "db" {
		t.Errorf("Name = %q, want the label", pm.Name)
	}
	if pm.Target() != "10.0.0.5:5432" {
		t.Errorf("Target() = %q, want the declared remote host", pm.Target())
	}
	if !strings.HasPrefix(pm.Key(), "static:db:") {
		t.Errorf("Key() = %q, want the label as its owner", pm.Key())
	}

	// A labelled range still needs one distinguishable row per port.
	ranged, err := ParseForward("api=8000-8001")
	if err != nil {
		t.Fatalf("ParseForward: %v", err)
	}
	if ranged[0].Name != "api:8000" || ranged[1].Name != "api:8001" {
		t.Errorf("names = %q, %q, want the port appended", ranged[0].Name, ranged[1].Name)
	}
}

func TestParseForwardRejectsBadSpecs(t *testing.T) {
	tests := map[string]string{
		"empty":                    "",
		"not a port":               "http",
		"empty label":              "=5432",
		"empty remote host":        "5432@",
		"descending range":         "8010-8000",
		"mismatched range lengths": "15432-15434:8000-8001",
		"port zero":                "0",
		"port out of range":        "70000",
		"oversized range":          "1000-2000",
		"missing remote port":      "15432:",
		// A half-typed range or local side must not quietly mean something else.
		"range with no upper bound": "8000-",
		"colon with no local port":  ":5432",
	}
	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			if maps, err := ParseForward(spec); err == nil {
				t.Errorf("ParseForward(%q) = %+v, want an error", spec, maps)
			}
		})
	}
}

func TestParseForwardsRejectsTheWholeSetOnOneBadSpec(t *testing.T) {
	// A typo in the third -forward is otherwise a row that silently never
	// appears, which is a much longer debugging session than a startup error.
	if _, err := ParseForwards([]string{"5432", "8080", "nonsense"}); err == nil {
		t.Fatal("ParseForwards accepted a bad spec")
	}
	maps, err := ParseForwards([]string{"5432", "15432:5432@10.0.0.5"})
	if err != nil {
		t.Fatalf("ParseForwards: %v", err)
	}
	if len(maps) != 2 {
		t.Errorf("maps = %+v, want one per spec", maps)
	}
}
