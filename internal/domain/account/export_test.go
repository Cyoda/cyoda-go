package account

// Re-exports for black-box tests in package account_test.
// This file is only compiled in test builds (per Go's _test.go convention).

var BoundedJSONDecodeForTesting = boundedJSONDecode
