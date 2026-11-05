// Package workerapi serves /worker/v1 for executors (docs/architecture.md,
// "Worker API"): claim (worker key, long-poll, capability gate), heartbeat
// and fail (attempt token, checked against the attempt in the path). sign
// lands in P1-T06 and complete in P1-T07. The payload never carries storage
// credentials; source URLs and content keys are added by providers at claim
// time (P1-T04, P1-T08).
package workerapi
