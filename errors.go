package idempotency

type idemError string

func (e idemError) Error() string { return string(e) }

const errRequired = idemError("idempotency: db and now are required")

// msgStorage is the body of the 500 answered when the record table cannot be read or written:
// nothing ran, so the client may retry.
const msgStorage = "idempotency: storage unavailable"
