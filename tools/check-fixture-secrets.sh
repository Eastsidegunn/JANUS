#!/bin/sh
# check-fixture-secrets.sh <검사 대상 디렉토리>
#
# T8 픽스처 커밋 전 비밀 검사 (fail-closed):
#   exit 0 — 무검출 (커밋 가능)
#   exit 1 — 비밀 검출 (해당 녹화 폐기·재녹화; 픽스처는 수정 금지라 마스킹 불가)
#   exit 2 — 사용법 오류·대상 부재·grep 실행 오류 (통과로 간주하지 않음)
#
# 패턴은 core/logd redaction 기본 집합과 동일하다.
set -u

if [ "$#" -ne 1 ]; then
	echo "사용법: $0 <검사 대상 디렉토리>" >&2
	exit 2
fi
target=$1
if [ ! -d "$target" ]; then
	echo "검사 대상이 디렉토리가 아니거나 존재하지 않음: $target" >&2
	exit 2
fi

matches=$(grep -rInE 'sk-ant-[A-Za-z0-9_-]{10,}|sk-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}|github_pat_|xox[baprs]-|BEGIN [A-Z ]*PRIVATE KEY|eyJ[A-Za-z0-9_-]{10,}\.eyJ' "$target")
status=$?
case $status in
0)
	echo '비밀 검출 — 커밋 금지:'
	echo "$matches"
	exit 1
	;;
1)
	echo "비밀 검사 통과: $target"
	exit 0
	;;
*)
	echo "grep 실행 오류 (exit $status) — 통과로 간주하지 않음" >&2
	exit 2
	;;
esac
