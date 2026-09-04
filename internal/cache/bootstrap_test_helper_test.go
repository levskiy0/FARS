package cache

import "context"

// Finite orchestration for assertions that require discovery, deletion and persistence.
func bootstrapForTest(m *Manager) error {
	ctx := context.Background()
	if err := m.bootstrapIndex(ctx); err != nil {
		return err
	}
	if err := m.drainPendingOnce(ctx); err != nil {
		return err
	}
	return m.flushManifest()
}
