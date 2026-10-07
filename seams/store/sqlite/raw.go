package sqlite

import (
	"database/sql"
	"encoding/base64"
)

// decodeRaw는 이벤트의 base64 raw를 BLOB 바이트로 바꾼다.
// nil(필드 생략) → nil, 빈 문자열(합성 이벤트) → 빈 BLOB.
func decodeRaw(raw *string) ([]byte, error) {
	if raw == nil {
		return nil, nil
	}
	b, err := base64.StdEncoding.DecodeString(*raw)
	if err != nil {
		return nil, err
	}
	if b == nil {
		b = []byte{}
	}
	return b, nil
}

// encodeRaw는 BLOB 컬럼을 base64 문자열 포인터로 되돌린다.
// NULL(필드 생략) → nil. 빈 BLOB(합성 이벤트의 "")은 드라이버가 nil 바이트로
// 스캔하므로, NULL 판별은 sql.Null의 Valid 플래그로만 한다 — 빈 raw와
// raw 부재의 구분이 왕복에서 소실되면 안 된다.
func encodeRaw(b sql.Null[[]byte]) *string {
	if !b.Valid {
		return nil
	}
	s := base64.StdEncoding.EncodeToString(b.V)
	return &s
}
