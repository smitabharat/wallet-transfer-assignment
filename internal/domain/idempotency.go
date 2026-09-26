package domain

import "time"

// IdempotencyRecord stores the outcome of the first request made with a key,
// so that retries can be answered with exactly the same result.
type IdempotencyRecord struct {
	Key         string
	RequestHash string
	TransferID  string
	// Response is the JSON snapshot of the transfer returned the first time.
	Response  []byte
	CreatedAt time.Time
}

// Matches reports whether req is the same request that created the record.
func (r IdempotencyRecord) Matches(req TransferRequest) bool {
	return r.RequestHash == req.Fingerprint()
}
