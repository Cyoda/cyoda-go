package workflow

// RecordEngineEvents exposes the engine-event driver to the external test
// package, which also drives the scheduler: the scheduler imports this
// package, so a test of this package cannot import it.
var RecordEngineEvents = recordEngineEvents
