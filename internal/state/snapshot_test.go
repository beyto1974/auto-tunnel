package state

import "testing"

func TestCounts(t *testing.T) {
	// PAUSED folds into listening (the port is still bound) and UNSUPPORTED into
	// broken (nothing will ever flow), which is what the header line promises.
	snap := Snapshot{Tunnels: []Tunnel{
		{State: TunnelActive},
		{State: TunnelActive},
		{State: TunnelListening},
		{State: TunnelPaused},
		{State: TunnelDegraded},
		{State: TunnelError},
		{State: TunnelUnsupported},
		{State: TunnelState("something new")}, // an unknown state must not be counted anywhere
	}}

	active, listening, degraded, broken := snap.Counts()

	if active != 2 {
		t.Errorf("active = %d, want 2", active)
	}
	if listening != 2 {
		t.Errorf("listening = %d, want 2 (LISTENING + PAUSED)", listening)
	}
	if degraded != 1 {
		t.Errorf("degraded = %d, want 1", degraded)
	}
	if broken != 2 {
		t.Errorf("broken = %d, want 2 (ERROR + UNSUPPORTED)", broken)
	}
}
