package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/TheIntroDB/plex-sync/internal/app"
	"github.com/TheIntroDB/plex-sync/internal/ledger"
	"github.com/TheIntroDB/plex-sync/internal/sync"
)

// A refresh is three loads, and the stage must survive all of them.
//
// It used to clear on whichever arrived first, and readiness never cleared it at
// all, so pressing r left the header saying "refreshing" for the rest of the
// session with nothing running.
func TestRefreshClearsOnlyAfterBothLoads(t *testing.T) {
	m := newTestModel(t)

	press(m, "r")
	if m.busy != "refreshing" {
		t.Fatalf("busy = %q, want refreshing right after the key", m.busy)
	}
	if !strings.Contains(m.View(), "refreshing") {
		t.Error("the header does not say it is refreshing")
	}

	// The first of the three comes back: still refreshing.
	m.Update(readinessMsg{readiness: app.Readiness{PlexOK: true, TIDBOK: true}})
	if m.busy != "refreshing" {
		t.Errorf("busy = %q after one of the three loads, want it to still be refreshing", m.busy)
	}

	// The second.
	m.Update(statsMsg{stats: &ledger.Stats{}})
	if m.busy != "refreshing" {
		t.Errorf("busy = %q after two of three, want still refreshing", m.busy)
	}

	// The third: done.
	m.Update(schedulerMsg{running: false})
	if m.busy != "" {
		t.Errorf("busy = %q after all three loads, want it cleared", m.busy)
	}
	if strings.Contains(m.View(), "refreshing") {
		t.Error("the header still says refreshing after all three loads came back")
	}
	if m.status != "refreshed" {
		t.Errorf("status = %q, want refreshed", m.status)
	}
}

// The order the three arrive in must not matter.
func TestRefreshClearsInEitherOrder(t *testing.T) {
	m := newTestModel(t)
	press(m, "r")

	m.Update(statsMsg{stats: &ledger.Stats{}})
	if m.busy != "refreshing" {
		t.Errorf("busy = %q after the ledger only, want refreshing", m.busy)
	}
	m.Update(readinessMsg{readiness: app.Readiness{PlexOK: true, TIDBOK: true}})
	if m.busy != "refreshing" {
		t.Errorf("busy = %q after ledger+readiness, want still refreshing", m.busy)
	}
	m.Update(schedulerMsg{running: false})
	if m.busy != "" {
		t.Errorf("busy = %q, want cleared", m.busy)
	}
	if m.status != "refreshed" {
		t.Errorf("status = %q, want refreshed", m.status)
	}
}

// A refresh that reloads the ledger must not overwrite what an action just
// reported. Before this, applying said "wrote 3 markers" and then the ledger
// came back and said "refreshed", so the outcome was never visible.
func TestAnActionStatusSurvivesTheLedgerReload(t *testing.T) {
	m := newTestModel(t)

	m.startLoad("writing", 1)
	m.Update(applyMsg{result: &sync.Result{}})
	after := m.status
	if !strings.Contains(after, "wrote") {
		t.Fatalf("status = %q, want the outcome of the write", after)
	}
	if m.busy != "" {
		t.Errorf("busy = %q, want cleared after the write", m.busy)
	}

	// The reload that follows the write.
	m.Update(statsMsg{stats: &ledger.Stats{}})
	if m.status != after {
		t.Errorf("status = %q, want it left alone as %q", m.status, after)
	}
}

// A failure has to end the stage too, or a service that is down leaves the
// interface waiting for something that will never arrive.
func TestAFailedRefreshStillEndsTheStage(t *testing.T) {
	m := newTestModel(t)
	press(m, "r")

	m.Update(statsMsg{stats: &ledger.Stats{}})
	m.Update(readinessMsg{readiness: app.Readiness{
		PlexOK:    false,
		PlexError: "connection refused",
	}})
	m.Update(schedulerMsg{running: false})

	if m.busy != "" {
		t.Errorf("busy = %q, want cleared after a failed check", m.busy)
	}
	if m.err == nil {
		t.Error("the failure was not reported")
	}
}

// A plan or an inventory load ends its own stage.
func TestSingleLoadStagesEnd(t *testing.T) {
	m := newTestModel(t)

	press(m, "p")
	if m.busy != "planning" {
		t.Fatalf("busy = %q, want planning", m.busy)
	}
	m.Update(planMsg{err: errors.New("no Plex")})
	if m.busy != "" {
		t.Errorf("busy = %q, want cleared after a failed plan", m.busy)
	}

	press(m, "l")
	if m.busy != "reading the library" {
		t.Fatalf("busy = %q, want reading the library", m.busy)
	}
	m.Update(inventoryMsg{})
	if m.busy != "" {
		t.Errorf("busy = %q, want cleared after the inventory came back", m.busy)
	}
}

// A result for a stage that has already ended must not invent a status, and must
// not leave anything stuck.
//
// Results are not tagged with their stage, so a stray one does end the running
// stage early. That costs an indicator and nothing else, which is why it is
// tolerated rather than fixed by threading a token through every message.
func TestResultsForAFinishedStageAreHarmless(t *testing.T) {
	m := newTestModel(t)

	// Nothing is running; a result arrives anyway.
	m.Update(statsMsg{stats: &ledger.Stats{}})
	if m.busy != "" {
		t.Errorf("busy = %q, want it to stay empty", m.busy)
	}
	if m.status == "refreshed" {
		t.Error("a result with nothing running reported a refresh")
	}

	// A result that arrives after its own stage ended does not stick.
	m.startLoad("planning", 1)
	m.Update(planMsg{})
	if m.busy != "" {
		t.Fatalf("busy = %q, want cleared", m.busy)
	}
	m.Update(statsMsg{stats: &ledger.Stats{}})
	if m.busy != "" {
		t.Errorf("busy = %q after a late result, want it to stay cleared", m.busy)
	}
}

// Starting up ends with "ready" rather than leaving "loading" on the screen, and
// a later refresh does not put it back.
func TestReadyOnlyOnTheWayIn(t *testing.T) {
	m := newTestModel(t)
	if m.status != "loading" {
		t.Fatalf("status = %q at startup", m.status)
	}

	m.Update(readinessMsg{readiness: app.Readiness{PlexOK: true, TIDBOK: true}})
	if m.status != "ready" {
		t.Errorf("status = %q after the first check, want ready", m.status)
	}

	// A refresh now reports itself, not readiness again.
	press(m, "r")
	m.Update(readinessMsg{readiness: app.Readiness{PlexOK: true, TIDBOK: true}})
	m.Update(statsMsg{stats: &ledger.Stats{}})
	m.Update(schedulerMsg{running: false})
	if m.status != "refreshed" {
		t.Errorf("status = %q after a refresh, want refreshed", m.status)
	}
}
