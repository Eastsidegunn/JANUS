// Package seams는 층 2다: 6개 seam(모델, 툴, 실행 세계, 영속화, 서브에이전트, UI)의 구현체들.
// 각 seam은 seams/<이름>/ 하위 패키지로 존재하며 contracts와 core만 의존할 수 있다.
// seam 간 수평 import는 금지다 (§3.1).
package seams
