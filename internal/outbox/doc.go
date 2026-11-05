// Package outbox publishes webhook and route_sync rows written in the same
// transaction as the state change. Webhooks are HMAC-SHA256 signed over the
// timestamp and body. Lands in P1-T07/T09.
package outbox
