package main

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/google/uuid"
)

// Client talks to agentd over the unix socket
type Client struct {
	conn net.Conn
	enc  *json.Encoder
	dec  *json.Decoder
}

// NewClient connects to the agentd socket
func NewClient(sockPath string) (*Client, error) {
	conn, err := net.DialTimeout("unix", sockPath, 3*time.Second)
	if err != nil {
		return nil, err
	}
	return &Client{
		conn: conn,
		enc:  json.NewEncoder(conn),
		dec:  json.NewDecoder(conn),
	}, nil
}

// Close closes the connection
func (c *Client) Close() error {
	return c.conn.Close()
}

// Call sends a command and waits for the response
func (c *Client) Call(command string, args map[string]interface{}) (*Response, error) {
	req := Request{
		ID:      uuid.NewString()[:8],
		Command: command,
		Args:    args,
	}
	if err := c.enc.Encode(&req); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}

	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var resp Response
	if err := c.dec.Decode(&resp); err != nil {
		return nil, fmt.Errorf("recv: %w", err)
	}
	return &resp, nil
}

// helpers to avoid import cycle in main
func jsonMarshal(v interface{}) ([]byte, error) { return json.Marshal(v) }
func jsonUnmarshal2(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
