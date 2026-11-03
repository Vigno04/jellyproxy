package telemetry

import (
	"testing"
	"time"
)

func TestSharedTitleAppearsOnceWithTwoClients(t *testing.T) {
	h := NewHub(5 * time.Second)
	names := NewItemNames()
	names.Set("ABCD-1234", "Inception")
	h.Sent("tv", "Living room", "abcd1234", 1000, false)
	h.Sent("phone", "Phone", "ABCD-1234", 500, true)
	h.Pulled("abcd1234", 1000)

	s := h.Snapshot(names)
	if len(s.Clients) != 2 || len(s.Items) != 1 {
		t.Fatalf("clients=%d items=%d", len(s.Clients), len(s.Items))
	}
	it := s.Items[0]
	if !it.Shared || it.Clients != 2 || it.Name != "Inception" {
		t.Fatalf("unexpected item %+v", it)
	}
	if s.TotalOut != 1500 || s.TotalPulled != 1000 {
		t.Fatalf("totals out=%d pulled=%d", s.TotalOut, s.TotalPulled)
	}
}

func TestHueIsStableAndIdNormalised(t *testing.T) {
	if Hue("AB-CD") != Hue("abcd") {
		t.Fatal("hue must ignore case and dashes")
	}
}

func TestNilHubIsSafe(t *testing.T) {
	var h *Hub
	h.Sent("a", "a", "i", 1, false)
	h.Pulled("i", 1)
}
