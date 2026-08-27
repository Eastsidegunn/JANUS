// Package collector는 효과 평면이다: 샌드박스 경계에서 에이전트 협조 없이 수집된
// 관측을 record 후보로 만드는 순수 라이브러리다. 표준 라이브러리와 contracts만
// 사용하며, core/seams와 코드 경로를 공유하지 않고 span_id로만 조인한다 (§3.1).
// 이 패키지는 로그에 쓰지 않으며 writer 조립은 surface의 책임이다.
package collector
