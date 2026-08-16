// Package validate는 contracts의 JSON Schema로 인스턴스를 런타임 검증한다.
// 어댑터 등록 게이트(§5.2)와 위반 샘플 거부(T1 완료 기준)의 구현 지점이며,
// 저장소 전체에서 외부 검증기(santhosh-tekuri/jsonschema)를 import하는
// 유일한 패키지다(docs/t1-codegen-proposal.md §6).
//
// 검증 진입점은 세 개뿐이다: ValidateRecord(이벤트 레코드),
// ValidateCommand(코어→어댑터), ValidateEvent(어댑터→코어).
// wire 문서의 루트는 정의 라이브러리이므로 범용 루트 검증은 제공하지 않는다.
package validate

import (
	"bytes"
	"fmt"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/Eastsidegunn/JANUS/contracts"
)

// Validators는 컴파일된 스키마 검증기 묶음이다. New로 생성한다.
type Validators struct {
	record  *jsonschema.Schema
	command *jsonschema.Schema
	event   *jsonschema.Schema
}

// New는 embed된 스키마 원문을 컴파일한다. 스키마가 저장소와 함께 배포되므로
// 실패는 프로그래밍 오류다 — 호출자는 부팅 시점에 한 번 호출한다.
func New() (*Validators, error) {
	c := jsonschema.NewCompiler()
	for file, id := range map[string]string{
		contracts.EventsSchemaFile: contracts.EventsSchemaID,
		contracts.WireSchemaFile:   contracts.WireSchemaID,
	} {
		b, err := contracts.SchemaFS.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("스키마 읽기 %s: %w", file, err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("스키마 파싱 %s: %w", file, err)
		}
		if err := c.AddResource(id, doc); err != nil {
			return nil, fmt.Errorf("스키마 등록 %s: %w", id, err)
		}
	}
	v := &Validators{}
	for _, e := range []struct {
		dst **jsonschema.Schema
		url string
	}{
		{&v.record, contracts.EventsSchemaID},
		{&v.command, contracts.WireSchemaID + "#/$defs/command"},
		{&v.event, contracts.WireSchemaID + "#/$defs/event"},
	} {
		s, err := c.Compile(e.url)
		if err != nil {
			return nil, fmt.Errorf("스키마 컴파일 %s: %w", e.url, err)
		}
		*e.dst = s
	}
	return v, nil
}

// ValidateRecord는 §5.1 이벤트 레코드 한 건을 검증한다.
func (v *Validators) ValidateRecord(data []byte) error {
	return validate(v.record, data)
}

// ValidateCommand는 코어→어댑터 메시지(§5.2 stdin) 한 줄을 검증한다.
func (v *Validators) ValidateCommand(data []byte) error {
	return validate(v.command, data)
}

// ValidateEvent는 어댑터→코어 메시지(§5.2 stdout) 한 줄을 검증한다.
func (v *Validators) ValidateEvent(data []byte) error {
	return validate(v.event, data)
}

func validate(schema *jsonschema.Schema, data []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("JSON 파싱: %w", err)
	}
	return schema.Validate(inst)
}
