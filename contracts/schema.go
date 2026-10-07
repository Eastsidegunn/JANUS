package contracts

import "embed"

// SchemaFS는 contracts의 JSON Schema 원문이다 — 이 파일들이 진실이고
// gen/의 Go 타입은 codegen 산출물이다. 런타임 검증 진입점은
// contracts/validate 패키지가 이 FS를 컴파일해 제공한다.
//
//go:embed events.schema.json wire.schema.json
var SchemaFS embed.FS

const (
	// EventsSchemaFile은 §5.1 이벤트 레코드 스키마 파일명이다.
	EventsSchemaFile = "events.schema.json"
	// WireSchemaFile은 §5.2 어댑터 와이어 프로토콜 스키마 파일명이다.
	WireSchemaFile = "wire.schema.json"

	// EventsSchemaID와 WireSchemaID는 각 문서의 $id다.
	EventsSchemaID = "urn:hx:contracts:events:v1"
	WireSchemaID   = "urn:hx:contracts:wire:v1"
)
