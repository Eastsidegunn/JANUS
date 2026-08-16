package main

import (
	"encoding/json"
	"fmt"
	"go/format"
	"sort"
	"strings"
)

// schemagen은 contracts의 JSON Schema를 Go 타입으로 변환한다.
// docs/t1-codegen-proposal.md §2의 승인 키워드 서브셋만 수용하며,
// 서브셋 밖 키워드·구조는 생성 실패다(fail-closed).

// ---- 계약 검사 ----

// allowedKeywords는 제안서 §2에서 확정된 세 범주의 합집합이다.
var allowedKeywords = map[string]bool{
	// (a) 형태 결정
	"$schema": true, "$id": true, "$defs": true, "$ref": true,
	"type": true, "properties": true, "required": true,
	"additionalProperties": true, "items": true, "enum": true,
	"const": true, "oneOf": true,
	// (b) 검증 전용 — 타입 형태에는 불참
	"format": true, "pattern": true, "minLength": true, "maxLength": true,
	"minimum": true, "maximum": true, "exclusiveMinimum": true,
	"exclusiveMaximum": true, "minItems": true, "maxItems": true,
	// (c) annotation
	"title": true, "description": true, "examples": true,
	"deprecated": true, "$comment": true,
}

var allowedTypes = map[string]bool{
	"object": true, "string": true, "integer": true, "array": true,
}

// checkContract는 스키마 노드를 재귀 순회하며 schemagen 계약 위반을 모은다.
func checkContract(node any, path string, errs *[]error) {
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	addErr := func(format string, a ...any) {
		*errs = append(*errs, fmt.Errorf("%s: %s", path, fmt.Sprintf(format, a...)))
	}
	for k, v := range m {
		switch {
		case !allowedKeywords[k]:
			addErr("서브셋 밖 키워드 %q", k)
		case k == "$ref":
			s, ok := v.(string)
			if !ok || !strings.HasPrefix(s, "#/$defs/") {
				addErr("$ref는 같은 문서의 #/$defs/만 허용: %v", v)
			}
		case k == "type":
			s, ok := v.(string)
			if !ok || !allowedTypes[s] {
				addErr("type은 object|string|integer|array 단일 문자열만 허용: %v", v)
			}
		case k == "additionalProperties":
			if b, ok := v.(bool); !ok || b {
				addErr("additionalProperties는 false만 허용: %v", v)
			}
		case k == "const":
			switch v.(type) {
			case string, json.Number:
			default:
				addErr("const는 문자열·정수만 허용: %v", v)
			}
		case k == "enum":
			list, ok := v.([]any)
			if !ok || len(list) == 0 {
				addErr("enum은 비어 있지 않은 배열이어야 함")
				continue
			}
			for _, e := range list {
				if _, ok := e.(string); !ok {
					addErr("enum 값은 문자열만 허용: %v", e)
				}
			}
		case k == "oneOf":
			branches, ok := v.([]any)
			if !ok {
				addErr("oneOf는 배열이어야 함")
				continue
			}
			for i, b := range branches {
				bp := fmt.Sprintf("%s/oneOf[%d]", path, i)
				bm, ok := b.(map[string]any)
				if !ok {
					*errs = append(*errs, fmt.Errorf("%s: 분기는 객체여야 함", bp))
					continue
				}
				if !branchHasConst(bm) {
					*errs = append(*errs, fmt.Errorf("%s: 판별 필드 const 없는 분기", bp))
				}
				checkContract(bm, bp, errs)
			}
		case k == "properties" || k == "$defs":
			sub, ok := v.(map[string]any)
			if !ok {
				addErr("%s는 객체여야 함", k)
				continue
			}
			for name, s := range sub {
				checkContract(s, path+"/"+k+"/"+name, errs)
			}
		case k == "items":
			checkContract(v, path+"/items", errs)
		}
	}
}

func branchHasConst(branch map[string]any) bool {
	props, _ := branch["properties"].(map[string]any)
	for _, p := range props {
		if pm, ok := p.(map[string]any); ok {
			if _, has := pm["const"]; has {
				return true
			}
		}
	}
	return false
}

// ---- 이름 규칙 ----

func exportName(s string) string {
	return camel(s, true)
}

// camel은 snake/kebab/slash 구분자를 CamelCase로 바꾼다. "id"는 "ID"로.
func camel(s string, upperFirst bool) string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-' || r == '/'
	})
	var b strings.Builder
	for i, p := range parts {
		if p == "id" && i > 0 {
			b.WriteString("ID")
			continue
		}
		if i == 0 && !upperFirst {
			b.WriteString(p)
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return b.String()
}

// ---- 타입 해석과 생성 ----

type generator struct {
	defs    map[string]any
	out     strings.Builder
	emitted []string // 방출된 최상위 타입·상수 이름 (파일 간 충돌 검사용)
	needJSON bool
}

type field struct {
	jsonName string
	goName   string
	goType   string
	optional bool
	raw      bool // json.RawMessage
}

// defKind는 $defs 엔트리의 생성 분류다.
func defKind(schema map[string]any) string {
	if _, ok := schema["enum"]; ok {
		return "enum"
	}
	if _, ok := schema["const"]; ok {
		return "const"
	}
	if t, _ := schema["type"].(string); t == "object" {
		return "struct"
	}
	return "primitive" // string/integer 제약 정의 — 인라인 해석, 방출 없음
}

// resolve는 스키마 노드를 Go 타입 문자열로 해석한다.
// ctx는 인라인 enum/객체의 합성 이름 접두사다.
func (g *generator) resolve(schema map[string]any, ctx string) (goType string, isRaw bool, err error) {
	if ref, ok := schema["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/$defs/")
		target, ok := g.defs[name].(map[string]any)
		if !ok {
			return "", false, fmt.Errorf("%s: 해석 불가 $ref %q", ctx, ref)
		}
		switch defKind(target) {
		case "struct", "enum":
			return exportName(name), false, nil
		case "const":
			return constBaseType(target["const"])
		default: // primitive
			return primitiveType(target, ctx)
		}
	}
	if _, ok := schema["enum"]; ok {
		// 인라인 enum → 합성 named 타입 (호출자가 사전에 emitInlineEnum으로 방출)
		return ctx, false, nil
	}
	if c, ok := schema["const"]; ok {
		return constBaseType(c)
	}
	switch t, _ := schema["type"].(string); t {
	case "integer":
		return "int64", false, nil
	case "string":
		return "string", false, nil
	case "array":
		items, ok := schema["items"].(map[string]any)
		if !ok {
			return "", false, fmt.Errorf("%s: items 없는 array", ctx)
		}
		if isInlineStruct(items) {
			itemName := ctx + "Item"
			if err := g.emitStruct(itemName, items); err != nil {
				return "", false, err
			}
			return "[]" + itemName, false, nil
		}
		if hasInlineEnum(items) {
			if err := g.emitInlineEnum(ctx+"Item", items); err != nil {
				return "", false, err
			}
			return "[]" + ctx + "Item", false, nil
		}
		it, _, err := g.resolve(items, ctx+"Item")
		if err != nil {
			return "", false, err
		}
		return "[]" + it, false, nil
	case "object":
		if _, hasProps := schema["properties"]; hasProps {
			if err := g.emitStruct(ctx, schema); err != nil {
				return "", false, err
			}
			return ctx, false, nil
		}
		g.needJSON = true
		return "json.RawMessage", true, nil
	default:
		return "", false, fmt.Errorf("%s: 해석 불가 스키마 (type=%v)", ctx, schema["type"])
	}
}

func isInlineStruct(schema map[string]any) bool {
	if _, isRef := schema["$ref"]; isRef {
		return false
	}
	t, _ := schema["type"].(string)
	_, hasProps := schema["properties"]
	return t == "object" && hasProps
}

func hasInlineEnum(schema map[string]any) bool {
	if _, isRef := schema["$ref"]; isRef {
		return false
	}
	_, has := schema["enum"]
	return has
}

func constBaseType(c any) (string, bool, error) {
	switch c.(type) {
	case string:
		return "string", false, nil
	case json.Number:
		return "int64", false, nil
	}
	return "", false, fmt.Errorf("지원하지 않는 const 값: %v", c)
}

func primitiveType(schema map[string]any, ctx string) (string, bool, error) {
	switch t, _ := schema["type"].(string); t {
	case "integer":
		return "int64", false, nil
	case "string":
		return "string", false, nil
	}
	return "", false, fmt.Errorf("%s: primitive 정의는 string|integer만 허용", ctx)
}

// mergedFields는 base properties와 oneOf 분기 properties를 병합한다.
// 필드 요구성: base required에 있거나 모든 분기에서 required면 필수, 아니면 옵셔널.
// 분기 전용 필드가 분기마다 const로 나타나면 그 합집합의 합성 enum이 된다.
func (g *generator) mergedFields(typeName string, schema map[string]any) ([]field, error) {
	baseProps, _ := schema["properties"].(map[string]any)
	baseReq := stringSet(schema["required"])
	branches, _ := schema["oneOf"].([]any)

	// 분기 전용 프로퍼티 수집
	type branchProp struct {
		schemas   []map[string]any
		inAll     bool
		consts    []string
		allConst  bool
		reqCount  int
	}
	branchProps := map[string]*branchProp{}
	for _, b := range branches {
		bm, _ := b.(map[string]any)
		props, _ := bm["properties"].(map[string]any)
		req := stringSet(bm["required"])
		for name, p := range props {
			if _, inBase := baseProps[name]; inBase {
				continue // base가 이긴다 — 분기는 검증 정제일 뿐
			}
			pm, _ := p.(map[string]any)
			bp := branchProps[name]
			if bp == nil {
				bp = &branchProp{allConst: true}
				branchProps[name] = bp
			}
			bp.schemas = append(bp.schemas, pm)
			if c, ok := pm["const"].(string); ok {
				bp.consts = append(bp.consts, c)
			} else {
				bp.allConst = false
			}
			if req[name] {
				bp.reqCount++
			}
		}
	}

	var fields []field
	addField := func(name string, pm map[string]any, required bool) error {
		ctx := typeName + exportName(name)
		if hasInlineEnum(pm) {
			if err := g.emitInlineEnum(ctx, pm); err != nil {
				return err
			}
		}
		t, isRaw, err := g.resolve(pm, ctx)
		if err != nil {
			return err
		}
		fields = append(fields, field{
			jsonName: name, goName: camel(name, true),
			goType: t, optional: !required, raw: isRaw,
		})
		return nil
	}

	// 중첩 타입(인라인 enum/struct)의 방출 순서까지 결정적이어야 하므로
	// map 순회는 반드시 이름 정렬 순으로 한다.
	for _, name := range sortedKeys(baseProps) {
		pm, _ := baseProps[name].(map[string]any)
		if err := addField(name, pm, baseReq[name]); err != nil {
			return nil, err
		}
	}
	for _, name := range sortedKeys(branchProps) {
		bp := branchProps[name]
		required := baseReq[name] || (len(branches) > 0 && bp.reqCount == len(branches))
		if bp.allConst && len(bp.consts) > 0 {
			// const 합집합 → 합성 enum
			enumName := typeName + exportName(name)
			g.emitEnumValues(enumName, bp.consts)
			fields = append(fields, field{
				jsonName: name, goName: camel(name, true),
				goType: enumName, optional: !required,
			})
			continue
		}
		// 모든 분기의 정의가 구조적으로 동일해야 한다
		first := canonical(bp.schemas[0])
		for _, s := range bp.schemas[1:] {
			if canonical(s) != first {
				return nil, fmt.Errorf("%s.%s: 분기 간 정의 불일치 — 병합 불가", typeName, name)
			}
		}
		if err := addField(name, bp.schemas[0], required); err != nil {
			return nil, err
		}
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].jsonName < fields[j].jsonName })
	return fields, nil
}

func (g *generator) emitStruct(name string, schema map[string]any) error {
	fields, err := g.mergedFields(name, schema)
	if err != nil {
		return err
	}
	g.emitted = append(g.emitted, name)
	fmt.Fprintf(&g.out, "type %s struct {\n", name)
	for _, f := range fields {
		t := f.goType
		tag := f.jsonName
		if f.optional {
			tag += ",omitempty"
			if !f.raw && !strings.HasPrefix(t, "[]") {
				t = "*" + t
			}
		}
		fmt.Fprintf(&g.out, "\t%s %s `json:\"%s\"`\n", f.goName, t, tag)
	}
	g.out.WriteString("}\n\n")
	return nil
}

func (g *generator) emitInlineEnum(name string, schema map[string]any) error {
	list, _ := schema["enum"].([]any)
	values := make([]string, 0, len(list))
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s: enum 값은 문자열만 허용", name)
		}
		values = append(values, s)
	}
	g.emitEnumValues(name, values)
	return nil
}

func (g *generator) emitEnumValues(name string, values []string) {
	g.emitted = append(g.emitted, name)
	fmt.Fprintf(&g.out, "type %s string\n\nconst (\n", name)
	for _, v := range values {
		fmt.Fprintf(&g.out, "\t%s%s %s = %q\n", name, exportName(v), name, v)
	}
	g.out.WriteString(")\n\n")
}

func (g *generator) emitConst(name string, schema map[string]any) error {
	t, _, err := constBaseType(schema["const"])
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	g.emitted = append(g.emitted, exportName(name))
	if t == "string" {
		fmt.Fprintf(&g.out, "const %s string = %q\n\n", exportName(name), schema["const"])
	} else {
		fmt.Fprintf(&g.out, "const %s int64 = %s\n\n", exportName(name), schema["const"])
	}
	return nil
}

// Generate는 스키마 문서 하나를 Go 소스로 변환한다.
// rootTypeName이 비어 있지 않으면 루트 객체도 그 이름의 struct로 생성한다.
func Generate(root map[string]any, sourceName, rootTypeName string) (src []byte, emitted []string, err error) {
	var errs []error
	checkContract(root, sourceName, &errs)
	if len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		return nil, nil, fmt.Errorf("schemagen 계약 위반 %d건:\n  %s", len(errs), strings.Join(msgs, "\n  "))
	}

	g := &generator{defs: map[string]any{}}
	if d, ok := root["$defs"].(map[string]any); ok {
		g.defs = d
	}

	names := make([]string, 0, len(g.defs))
	for n := range g.defs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		schema, _ := g.defs[n].(map[string]any)
		switch defKind(schema) {
		case "struct":
			if err := g.emitStruct(exportName(n), schema); err != nil {
				return nil, nil, err
			}
		case "enum":
			if err := g.emitInlineEnum(exportName(n), schema); err != nil {
				return nil, nil, err
			}
		case "const":
			if err := g.emitConst(n, schema); err != nil {
				return nil, nil, err
			}
		}
	}
	if rootTypeName != "" {
		if err := g.emitStruct(rootTypeName, root); err != nil {
			return nil, nil, err
		}
	}

	var file strings.Builder
	fmt.Fprintf(&file, "// Code generated by tools/schemagen from %s; DO NOT EDIT.\n", sourceName)
	file.WriteString("// 생성물 — 손으로 수정 금지(CLAUDE.md). 진실은 contracts의 JSON Schema다.\n\n")
	file.WriteString("package gen\n\n")
	if g.needJSON {
		file.WriteString("import \"encoding/json\"\n\n")
	}
	file.WriteString(g.out.String())

	formatted, err := format.Source([]byte(file.String()))
	if err != nil {
		return nil, nil, fmt.Errorf("생성 소스 gofmt 실패: %w\n%s", err, file.String())
	}
	return formatted, g.emitted, nil
}

// ---- 유틸 ----

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func stringSet(v any) map[string]bool {
	out := map[string]bool{}
	if list, ok := v.([]any); ok {
		for _, e := range list {
			if s, ok := e.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}

// canonical은 타입 형태에 불참하는 키워드(annotation + 검증 전용)를 제거한
// 정렬 JSON 직렬화다 — 분기 간 병합 가능성의 구조 비교용. 검증 전용 키워드는
// Go 타입에 반영되지 않으므로(제안서 §2 범주 b) 분기마다 달라도 병합 가능하다.
func canonical(node any) string {
	b, _ := json.Marshal(stripNonShape(node))
	return string(b)
}

var nonShapeKeywords = map[string]bool{
	"description": true, "$comment": true, "title": true, "examples": true,
	"deprecated": true,
	"format": true, "pattern": true, "minLength": true, "maxLength": true,
	"minimum": true, "maximum": true, "exclusiveMinimum": true,
	"exclusiveMaximum": true, "minItems": true, "maxItems": true,
}

func stripNonShape(node any) any {
	switch n := node.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, v := range n {
			if nonShapeKeywords[k] {
				continue
			}
			out[k] = stripNonShape(v)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, v := range n {
			out[i] = stripNonShape(v)
		}
		return out
	}
	return node
}
