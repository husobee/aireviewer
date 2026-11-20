// SPDX-License-Identifier: MIT

package sandbox

import (
	"slices"
	"strings"
	"testing"
)

func TestClassifyAndDelta(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{Pass}, ClassPass},
		{[]string{Fail, Fail, Fail, Fail}, ClassFail},
		{[]string{Fail, Pass, Fail, Fail}, ClassFlaky},
		{[]string{Timeout, Pass}, ClassFlaky},
		{[]string{Timeout, Timeout}, ClassTimeout},
		{[]string{Skip}, ClassSkip},
		{nil, ClassAbsent},
	}
	for _, c := range cases {
		if got := Classify(c.in); got != c.want {
			t.Errorf("Classify(%v) = %s, want %s", c.in, got, c.want)
		}
	}
	deltas := map[[2]string]string{
		{ClassPass, ClassFail}:        "new_failure",
		{ClassAbsent, ClassFail}:      "new_failure",
		{ClassFail, ClassFail}:        "still_failing",
		{ClassFail, ClassPass}:        "fixed",
		{ClassPass, ClassFlaky}:       "flaky",
		{ClassPass, ClassPass}:        "pass",
		{ClassAbsent, ClassPass}:      "new_passing",
		{ClassPass, ClassAbsent}:      "removed",
		{ClassPass, ClassTimeout}:     "new_timeout",
		{ClassFlaky, ClassFail}:       "fails_now_flaky_before",
		{ClassSetupFailed, ClassPass}: "not_comparable",
	}
	for in, want := range deltas {
		if got := Delta(in[0], in[1]); got != want {
			t.Errorf("Delta(%s, %s) = %s, want %s", in[0], in[1], got, want)
		}
	}
}

func TestParseGoTestJSON(t *testing.T) {
	out := `{"Action":"run","Package":"ex/a","Test":"TestOK"}
{"Action":"pass","Package":"ex/a","Test":"TestOK","Elapsed":0}
{"Action":"fail","Package":"ex/a","Test":"TestBad","Elapsed":0}
{"Action":"fail","Package":"ex/a","Elapsed":0.1}
{"Action":"skip","Package":"ex/a","Test":"TestSkip"}
# ex/b [build failed]
{"Action":"fail","Package":"ex/b","Elapsed":0}
not json at all
`
	got, ok := ParseGoTestJSON([]byte(out))
	if !ok {
		t.Fatal("no events")
	}
	want := map[string]string{"ex/a.TestOK": Pass, "ex/a.TestBad": Fail, "ex/a.TestSkip": Skip, "ex/b [package]": Fail}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := ParseGoTestJSON([]byte("PASS\nok ex 0.1s\n")); ok {
		t.Error("plain output parsed as JSON")
	}
}

func TestParseLint(t *testing.T) {
	golangci := `{"Issues":[{"FromLinter":"errcheck","Text":"Error return value is not checked","Severity":"","Pos":{"Filename":"store/ttl.go","Line":40,"Column":2}},
{"FromLinter":"errcheck","Text":"Error return value is not checked","Pos":{"Filename":"store/ttl.go","Line":40,"Column":2}}],"Report":{}}`
	fs := ParseLint([]byte("level=info msg=x\n" + golangci + "\n"))
	if len(fs) != 1 || fs[0].Linter != "errcheck" || fs[0].File != "store/ttl.go" || fs[0].Line != 40 {
		t.Errorf("golangci = %+v", fs)
	}
	plain := "./store/ttl.go:12:5: printf: wrong arg\n/work/store/x.go:3: unused variable\nsome noise: here\nError: exit 1\n"
	fs = ParseLint([]byte(plain))
	if len(fs) != 2 || fs[0].File != "store/ttl.go" || fs[0].Column != 5 || fs[1].File != "store/x.go" || fs[1].Line != 3 {
		t.Errorf("plain = %+v", fs)
	}
}

func TestArgsAreLockedDown(t *testing.T) {
	a := Args(Spec{Name: "n", Image: "alpine:3", Cmd: "go test ./...", Workdir: "/work", User: "65534:65534",
		Env: map[string]string{"B": "2", "A": "1"}, Memory: "4g", CPUs: "2",
		Mounts: []Mount{{Source: "/host/src", Target: "/src", ReadOnly: true}, {Source: "vol", Target: "/work", Volume: true}}})
	s := strings.Join(a, " ")
	for _, want := range []string{"--rm", "--cap-drop ALL", "--security-opt no-new-privileges", "--read-only", "--network none",
		"--pids-limit 512", "--memory 4g --memory-swap 4g", "--cpus 2", "--user 65534:65534",
		"--mount type=bind,source=/host/src,target=/src,readonly", "--mount type=volume,source=vol,target=/work",
		"--env A=1 --env B=2", "--entrypoint /bin/sh alpine:3 -c go test ./..."} {
		if !strings.Contains(s, want) {
			t.Errorf("args lack %q:\n%s", want, s)
		}
	}
	if slices.Contains(a, "--privileged") || strings.Contains(s, "docker.sock") {
		t.Errorf("unsafe args: %s", s)
	}
	if a[len(a)-1] != "go test ./..." {
		t.Errorf("command must be one argv element: %q", a[len(a)-1])
	}
	if net := strings.Join(Args(Spec{Name: "n", Image: "i", Cmd: "c", Network: true}), " "); !strings.Contains(net, "--network bridge") {
		t.Errorf("network on: %s", net)
	}
}
