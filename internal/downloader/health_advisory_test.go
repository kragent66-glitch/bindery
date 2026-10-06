package downloader

import (
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// TestHealthStoreAdvisorySurvivesProbe pins that the 15 minute path probe,
// which rewrites the client's health with Set, does not erase the advisory
// the importer records when it pauses automatic blocklisting (#3024).
func TestHealthStoreAdvisorySurvivesProbe(t *testing.T) {
	store := NewHealthStore()
	store.SetAdvisory(7, models.DownloadClientHealth{Status: HealthError, Message: "paused"})
	store.Set(7, models.DownloadClientHealth{Status: HealthOK, Message: "paths fine"})
	if h := store.Get(7); h == nil || h.Message != "paused" {
		t.Fatalf("after a passing probe = %+v, want the advisory", h)
	}
	store.Set(7, models.DownloadClientHealth{Status: HealthError, Message: "path missing."})
	if h := store.Get(7); h == nil || h.Message != "path missing. paused" {
		t.Fatalf("after a failing probe = %+v, want both messages", h)
	}
	store.ClearAdvisory(7)
	if h := store.Get(7); h == nil || h.Message != "path missing." {
		t.Fatalf("after ClearAdvisory = %+v, want the probe result", h)
	}
}

// TestHealthStoreAdvisoryNotifiesOnce pins that the advisory publishes the
// health event on entry only, like Set's entry into HealthError.
func TestHealthStoreAdvisoryNotifiesOnce(t *testing.T) {
	spy := &healthSpy{}
	store := NewHealthStore().WithNotifier(spy)
	store.SetAdvisory(7, models.DownloadClientHealth{Status: HealthError, Message: "paused"})
	store.SetAdvisory(7, models.DownloadClientHealth{Status: HealthError, Message: "paused"})
	if len(spy.calls) != 1 || spy.calls[0].eventType != notifierEventHealth {
		t.Fatalf("health events = %+v, want exactly one", spy.calls)
	}
}
