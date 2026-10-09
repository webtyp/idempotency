package idempotency


type idemError string

func (e idemError) Error() string { return string(e) }

const errMissingDBAndNow = idemError("idempotency: db and now are required")
