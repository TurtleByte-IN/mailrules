package imap

import (
	"encoding/json"
	"errors"
	"fmt"
)

// errTokenRefused is the server refusing an XOAUTH2 access token. Gmail and Outlook do it
// with a challenge carrying a JSON description of the error, which the client must answer
// before the server says NO; the client stops there instead, and the connection is closed.
var errTokenRefused = errors.New("the server refused the access token")

// xoauth2 is the SASL XOAUTH2 mechanism Gmail and Outlook take over IMAP: the address and
// an OAuth 2.0 access token in one initial response
// (https://developers.google.com/workspace/gmail/imap/xoauth2-protocol).
type xoauth2 struct{ user, token string }

func (x xoauth2) Start() (string, []byte, error) {
	return "XOAUTH2", []byte("user=" + x.user + "\x01auth=Bearer " + x.token + "\x01\x01"), nil
}

// Next is only called with the server's error challenge. Its status (such as "401") is
// kept; the rest names the scopes the token lacked, never the token.
func (x xoauth2) Next(challenge []byte) ([]byte, error) {
	var e struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(challenge, &e) == nil && e.Status != "" {
		return nil, fmt.Errorf("%w (status %s)", errTokenRefused, e.Status)
	}
	return nil, errTokenRefused
}
