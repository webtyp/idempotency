package idempotency

import (
	"webtyp.com/json"
	"webtyp.com/model"
	"webtyp.com/router"
)

const contentTypeJSON = "application/json"

// capturingContext forwards the response to the embedded context and keeps a copy of status,
// body and Content-Type so the middleware can store them.
type capturingContext struct {
	router.Context
	status      int
	body        []byte
	contentType string
}

func (c *capturingContext) WriteStatus(status int) {
	c.status = status
	c.Context.WriteStatus(status)
}

func (c *capturingContext) Write(b []byte) (int, error) {
	c.body = append(c.body, b...)
	return c.Context.Write(b)
}

func (c *capturingContext) SetHeader(k, v string) {
	if k == headerContentType {
		c.contentType = v
	}
	c.Context.SetHeader(k, v)
}

// Encode goes through this context's own Write so the body is captured; the embedded Encode
// would write straight to the transport.
func (c *capturingContext) Encode(v model.Encodable) error {
	var out []byte
	if err := json.Encode(v, &out); err != nil {
		return err
	}
	c.SetHeader(headerContentType, contentTypeJSON)
	_, err := c.Write(out)
	return err
}
