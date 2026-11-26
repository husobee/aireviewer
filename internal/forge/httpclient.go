// SPDX-License-Identifier: MIT

package forge

import (
	"errors"
	"net/http"
	"time"
)

// SameHostClient returns a copy of hc (or a new client when hc is nil) that
// follows redirects only within the original scheme and host.
//
// Go strips Authorization and Cookie on cross-domain redirects but forwards
// any other header, and GitLab authenticates with Private-Token / Job-Token
// headers. Refusing cross-host redirects keeps every credential header on the
// host it was issued for, whatever the client library does. A refused
// redirect is returned unfollowed (http.ErrUseLastResponse), so the client
// library reports the 3xx as an API error instead of retrying it.
func SameHostClient(hc *http.Client) *http.Client {
	var c http.Client
	if hc != nil {
		c = *hc
	} else {
		c.Timeout = 60 * time.Second
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		orig := via[0].URL
		if req.URL.Host != orig.Host || (orig.Scheme == "https" && req.URL.Scheme != "https") {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return &c
}
