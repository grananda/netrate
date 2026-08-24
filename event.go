package main

// Event is what the engine publishes while it works.
//
// The unexported event() method seals the interface: only the types below can
// satisfy it, so a type switch over an Event has a known, closed set of cases.
type Event interface{ event() }

type (
	PhaseEvent  struct{ Phase string }  // the engine moved on to a new stage
	SampleEvent struct{ Sample Sample } // one instantaneous speed reading
	DoneEvent   struct{ Result Result } // the run finished; this is the verdict
	ErrEvent    struct{ Err error }     // the run failed and produced nothing
)

func (PhaseEvent) event()  {}
func (SampleEvent) event() {}
func (DoneEvent) event()   {}
func (ErrEvent) event()    {}

// Phase labels, shared so the engine and the UI cannot drift apart.
const (
	phaseStarting   = "starting"
	phaseConnecting = "connecting"
	phaseMeasuring  = "measuring download"
	phaseComplete   = "complete"
	phaseFailed     = "error"
)
