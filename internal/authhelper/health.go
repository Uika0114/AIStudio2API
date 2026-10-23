package authhelper

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// Health authenticates the narrow RPC server without selecting an account or Google.
func (c *Client) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/healthz", nil)
	if err != nil {
		return failure("INVALID_REQUEST")
	}
	response, err := c.http.Do(req)
	if err != nil {
		return failure("HELPER_UNAVAILABLE")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return failure("HELPER_UNAVAILABLE")
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4097))
	decoder.DisallowUnknownFields()
	var value struct {
		Protocol int  `json:"protocol"`
		Ready    bool `json:"ready"`
	}
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.Protocol != Protocol || !value.Ready {
		return failure("HELPER_UNAVAILABLE")
	}
	return nil
}
