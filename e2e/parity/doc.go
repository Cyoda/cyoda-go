// Package parity holds the shared end-to-end test scenarios that every
// cyoda-go storage backend must satisfy. The scenarios are exposed as
// exported Run... functions taking *testing.T and a BackendFixture, plus
// a registry (AllTests) that per-backend wrappers iterate to run every
// scenario against their backend.
//
// The package depends only on stdlib, the generated OpenAPI client, and
// the generated gRPC/CloudEvents client. It does not import any package
// under internal/, so it is importable by an external Go module — in
// particular by out-of-tree storage plugins that want to validate their
// own backend against the same scenarios.
//
// TODO(plan-6-or-later): The following raw-store scenarios are now
// covered at the SPI layer by spitest.StoreFactoryConformance in
// github.com/cyoda/cyoda-go-spi/spitest, and are candidates
// for retirement from this HTTP-stack parity suite once spitest has
// been in production for a release cycle:
//
//   - EntityCreateAndGet
//   - EntityDelete
//   - EntityListByModel
//   - TemporalPointInTimeRetrieval
//   - TemporalGetAsAtPopulatesFullMeta
//   - TenantIsolationEntities
//   - TenantIsolationModels
//   - DeepSchemaSymmetry
//   - EmptyTenantOperations
//   - ConcurrentConflictingUpdate
//   - ConcurrentTransitionsDifferentEntities
//
// These are kept here for now to preserve HTTP-layer coverage
// (router, serialization, auth middleware) and to avoid removing
// green coverage during a refactor. A future plan should either
// slim them into thin HTTP smoke wrappers or remove them entirely
// once the SPI-layer coverage is proven.
package parity
