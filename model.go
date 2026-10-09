package idempotency

import "webtyp.com/model"

var RecordModel = model.Definition{
    Name: "idempotency_record",
    Fields: model.Fields{
        {Name: "id", Type: model.Text(), DB: &model.FieldDB{PK: true}}, // scope + "\x1f" + key
        {Name: "method", Type: model.Text(), NotNull: true},
        {Name: "path", Type: model.Text(), NotNull: true},
        {Name: "state", Type: model.Text(), NotNull: true},   // "in_progress" | "done"
        {Name: "status", Type: model.Int(), NotNull: true},   // stored response status (done)
        {Name: "body", Type: model.Blob()},                   // stored response body (done)
        {Name: "content_type", Type: model.Text()},           // stored Content-Type (done)
        {Name: "created_at", Type: model.Int(), NotNull: true}, // unix ns
    },
}

// Record is a placeholder until ormc successfully runs.
type Record struct {
	ID          string `json:"id"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	State       string `json:"state"`
	Status      int    `json:"status"`
	Body        []byte `json:"body"`
	ContentType string `json:"content_type"`
	CreatedAt   int    `json:"created_at"`
}
