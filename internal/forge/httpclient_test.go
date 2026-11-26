// SPDX-License-Identifier: MIT

package forge

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSameHostClientRefusesCrossHostRedirect(t *testing.T) {
	leaked := false
	evil := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Private-Token") != "" {
			leaked = true
		}
	}))
	defer evil.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/same" {
			http.Redirect(w, r, "/ok", http.StatusFound)
			return
		}
		if r.URL.Path == "/ok" {
			return
		}
		http.Redirect(w, r, evil.URL+"/steal", http.StatusFound)
	}))
	defer good.Close()

	c := SameHostClient(nil)
	req, _ := http.NewRequest(http.MethodGet, good.URL+"/x", nil)
	req.Header.Set("Private-Token", "secret")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("cross-host redirect was followed (status %d)", resp.StatusCode)
	}
	if leaked {
		t.Fatal("token reached the redirect target")
	}
	resp, err = c.Get(good.URL + "/same")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("same-host redirect not followed: %v", err)
	}
	_ = resp.Body.Close()
}

func TestSameHostClientDoesNotMutateInput(t *testing.T) {
	in := &http.Client{}
	_ = SameHostClient(in)
	if in.CheckRedirect != nil {
		t.Fatal("input client was modified")
	}
}
