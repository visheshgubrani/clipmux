package accounts

import "time"

// Failed logins are limited before any password is hashed: five per address
// and twenty overall in a sliding 15-minute window.
const (
	limitWindow = 15 * time.Minute
	limitPerIP  = 5
	limitAll    = 20
)
