// SPDX-License-Identifier: MIT

package schema

import (
	"fmt"
	"strings"
	"testing"
)

func TestMemberEvidence(t *testing.T) {
	setEnv := Env{
		HeadSHA: "abcdef1234567",
		MemberLineCount: func(member, side, path string) (int, error) {
			if member == "run-api" && path == "api/v1.go" {
				return 40, nil
			}
			return 0, fmt.Errorf("no such file")
		},
		AnchorOK: func(a string) bool { return a == "unit:github.com/o/api:api/v1.go#Get" },
	}
	rep := `{"agent":"changeset-analyst","version":1,"head_sha":"abcdef1234567","status":"ok","summary":"s","claims":[
	 {"id":"S1","statement":"client uses removed Get","category":"interface","severity":"major","confidence":"high",
	  "evidence":[{"kind":"code","file":"api/v1.go","lines":[10,12],"member":"run-api"}],
	  "anchors":["unit:github.com/o/api:api/v1.go#Get","unit:a.go#F"]},
	 {"id":"S2","statement":"no member","category":"interface","severity":"minor","confidence":"low",
	  "evidence":[{"kind":"code","file":"api/v1.go","lines":[1,2]}]},
	 {"id":"S3","statement":"bad member","category":"interface","severity":"minor","confidence":"low",
	  "evidence":[{"kind":"code","file":"api/v1.go","lines":[1,2],"member":"run-other"}]},
	 {"id":"S4","statement":"past end","category":"interface","severity":"minor","confidence":"low",
	  "evidence":[{"kind":"code","file":"api/v1.go","lines":[39,41],"member":"run-api"}]}]}`
	res, err := Validate([]byte(rep), setEnv)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Report.Claims) != 1 || res.Report.Claims[0].ID != "S1" || len(res.Dropped) != 3 {
		t.Fatalf("kept %+v dropped %+v", res.Report.Claims, res.Dropped)
	}
	if a := res.Report.Claims[0].Anchors; len(a) != 1 || a[0] != "unit:github.com/o/api:api/v1.go#Get" {
		t.Errorf("anchors = %v", a)
	}
	reasons := fmt.Sprint(res.Dropped)
	for _, want := range []string{"must name its member run", "member run-other", "exceed"} {
		if !strings.Contains(reasons, want) {
			t.Errorf("reasons lack %q: %s", want, reasons)
		}
	}

	// A normal PR report may not use member.
	single := strings.ReplaceAll(`{"agent":"code-reviewer","version":1,"head_sha":"abcdef1234567","status":"ok","summary":"s","claims":[
	 {"id":"F1","statement":"x","category":"defect","severity":"minor","confidence":"low",
	  "evidence":[{"kind":"code","file":"a.go","lines":[1,2],"member":"run-api"}]}]}`, "\t", "")
	res, err = Validate([]byte(single), env())
	if err != nil || len(res.Report.Claims) != 0 || !strings.Contains(fmt.Sprint(res.Dropped), "only allowed in change-set reports") {
		t.Errorf("member in PR report: %+v, %v", res, err)
	}
}
