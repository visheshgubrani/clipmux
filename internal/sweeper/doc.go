// Package sweeper runs periodic recovery: abandoned uploads (24 h of
// inactivity), lost leases, failed-attempt file cleanup and the route
// reconciler. Lands with the tasks that need it.
package sweeper
