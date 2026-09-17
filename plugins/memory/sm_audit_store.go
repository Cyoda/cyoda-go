package memory

import (
	"context"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

type StateMachineAuditStore struct {
	tenant  spi.TenantID
	factory *StoreFactory
}

func (s *StateMachineAuditStore) Record(ctx context.Context, entityID string, event spi.StateMachineEvent) error {
	s.factory.smAuditMu.Lock()
	defer s.factory.smAuditMu.Unlock()
	if s.factory.smAudit[s.tenant] == nil {
		s.factory.smAudit[s.tenant] = make(map[string][]spi.StateMachineEvent)
	}
	cp := copyEvent(event)
	s.factory.smAudit[s.tenant][entityID] = append(s.factory.smAudit[s.tenant][entityID], cp)
	return nil
}

func (s *StateMachineAuditStore) GetEvents(ctx context.Context, entityID string) ([]spi.StateMachineEvent, error) {
	s.factory.smAuditMu.RLock()
	defer s.factory.smAuditMu.RUnlock()
	tenantData, ok := s.factory.smAudit[s.tenant]
	if !ok {
		return []spi.StateMachineEvent{}, nil
	}
	events, ok := tenantData[entityID]
	if !ok {
		return []spi.StateMachineEvent{}, nil
	}
	return copyEvents(events), nil
}

func (s *StateMachineAuditStore) GetEventsByTransaction(ctx context.Context, entityID string, transactionID string) ([]spi.StateMachineEvent, error) {
	s.factory.smAuditMu.RLock()
	defer s.factory.smAuditMu.RUnlock()
	tenantData, ok := s.factory.smAudit[s.tenant]
	if !ok {
		return []spi.StateMachineEvent{}, nil
	}
	events, ok := tenantData[entityID]
	if !ok {
		return []spi.StateMachineEvent{}, nil
	}
	filtered := []spi.StateMachineEvent{}
	for _, e := range events {
		if e.TransactionID == transactionID {
			filtered = append(filtered, copyEvent(e))
		}
	}
	return filtered, nil
}

// stampAuditEventsForTx moves every recorded event LABELLED with txID onto
// instant — the commit instant of the transaction being committed.
//
// An audit event's timestamp is the clock of whichever process recorded it,
// read while the transaction was still open. Reporting that value leaves the
// audit trail on a different clock from the version history it accompanies,
// and able to invert against it; every backend therefore reports the commit
// instant instead, and a parity scenario holds the three to it.
//
// "Labelled with", not "written by": the engine records some events under a
// cascade entry's transaction id rather than the recording transaction's
// (EmitTransitionAborted). Such an event is stamped here only if its label
// names a transaction that later commits, matching what the SQL backends do
// with the same WHERE.
//
// Called from Commit inside the factory's entityMu critical section. That
// establishes entityMu → smAuditMu as a lock order; no path takes them in the
// opposite order (the audit store's own methods take smAuditMu alone), so it
// introduces no cycle.
func (f *StoreFactory) stampAuditEventsForTx(tenant spi.TenantID, txID string, instant time.Time) {
	if txID == "" {
		return
	}
	f.smAuditMu.Lock()
	defer f.smAuditMu.Unlock()
	for _, events := range f.smAudit[tenant] {
		for i := range events {
			if events[i].TransactionID == txID {
				events[i].Timestamp = instant
			}
		}
	}
}

func copyEvent(e spi.StateMachineEvent) spi.StateMachineEvent {
	cp := e
	if e.Data != nil {
		cp.Data = make(map[string]any, len(e.Data))
		for k, v := range e.Data {
			cp.Data[k] = v
		}
	}
	return cp
}

func copyEvents(events []spi.StateMachineEvent) []spi.StateMachineEvent {
	out := make([]spi.StateMachineEvent, len(events))
	for i, e := range events {
		out[i] = copyEvent(e)
	}
	return out
}
