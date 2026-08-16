package contracts

import (
	"encoding/json"
	"reflect"
	"testing"
)

// donePayload는 서브셋의 문서 간 $ref 금지 때문에 두 스키마에 거울 정의된다.
// 두 정의의 canonical 구조(annotation 제외)가 동일함을 고정한다 — [H] 승인 조건.
func TestDonePayloadMirrorsAreIdentical(t *testing.T) {
	events := loadDef(t, EventsSchemaFile, "subagentDonePayload")
	wire := loadDef(t, WireSchemaFile, "donePayload")
	if !reflect.DeepEqual(stripAnnotations(events), stripAnnotations(wire)) {
		t.Errorf("거울 정의 불일치:\nevents=%v\nwire=%v",
			stripAnnotations(events), stripAnnotations(wire))
	}
}

func loadDef(t *testing.T, file, def string) map[string]any {
	t.Helper()
	b, err := SchemaFS.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	defs, _ := root["$defs"].(map[string]any)
	d, ok := defs[def].(map[string]any)
	if !ok {
		t.Fatalf("%s: $defs.%s 없음", file, def)
	}
	return d
}

func stripAnnotations(node any) any {
	switch n := node.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, v := range n {
			switch k {
			case "description", "$comment", "title", "examples":
				continue
			}
			out[k] = stripAnnotations(v)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, v := range n {
			out[i] = stripAnnotations(v)
		}
		return out
	}
	return node
}
