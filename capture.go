package idempotency

import (
	"webtyp.com/json"
	"webtyp.com/model"
	"webtyp.com/router"
)

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
	if k == "Content-Type" {
		c.contentType = v
	}
	c.Context.SetHeader(k, v)
}

func (c *capturingContext) Encode(v model.Encodable) error {
	b, err := json.Encode(v)
	if err != nil {
		return err
	}
	c.SetHeader("Content-Type", "application/json")
	_, err = c.Write(b)
	return err
}
