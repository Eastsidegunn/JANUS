#!/bin/sh
# check-fixture-manifest.sh <픽스처 루트> [최소 녹화 수]
#
# T8 픽스처의 구성 완결성 검사 (fail-closed):
#   exit 0 — 전부 충족
#   exit 1 — 검사 실패 (README 부재 / meta 누락 / 최소 개수 미달)
#   exit 2 — 사용법 오류·대상 부재
#
# 검사 항목:
#   - <루트>/README.md 존재
#   - 모든 NN-슬러그.ndjson에 대응하는 NN-슬러그.meta.txt 존재
#   - meta만 있고 녹화가 없는 항목(skip)은 별도 목록으로 보고 — 개수에 미포함
#   - 실제 NDJSON 수가 최소 녹화 수(기본 15) 이상
set -u

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
	echo "사용법: $0 <픽스처 루트> [최소 녹화 수]" >&2
	exit 2
fi
root=$1
min=${2:-15}
if [ ! -d "$root" ]; then
	echo "대상이 디렉토리가 아니거나 존재하지 않음: $root" >&2
	exit 2
fi
case $min in
*[!0-9]* | '')
	echo "최소 녹화 수는 정수여야 함: $min" >&2
	exit 2
	;;
esac

failed=0

if [ ! -f "$root/README.md" ]; then
	echo "README.md 없음: $root/README.md — 녹화 목록 표를 작성하라" >&2
	failed=1
fi

recordings=0
for ndjson in $(find "$root" -type f -name '*.ndjson' | sort); do
	recordings=$((recordings + 1))
	meta=$(echo "$ndjson" | sed 's/\.ndjson$/.meta.txt/')
	if [ ! -f "$meta" ]; then
		echo "meta 누락: $ndjson 에 대응하는 $(basename "$meta") 없음" >&2
		failed=1
	fi
done

# meta만 있고 녹화가 없는 항목 = skip. 실패는 아니지만 개수에 넣지 않는다.
skips=""
for meta in $(find "$root" -type f -name '*.meta.txt' | sort); do
	ndjson=$(echo "$meta" | sed 's/\.meta\.txt$/.ndjson/')
	if [ ! -f "$ndjson" ]; then
		skips="$skips $meta"
	fi
done
if [ -n "$skips" ]; then
	echo "meta-only skip (녹화 수에 미포함):"
	for s in $skips; do echo "  $s"; done
fi

if [ "$recordings" -lt "$min" ]; then
	echo "녹화 ${recordings}건 — 최소 ${min}건 필요 (skip 제외)" >&2
	failed=1
fi

if [ "$failed" -ne 0 ]; then
	exit 1
fi
echo "매니페스트 검사 통과: 녹화 ${recordings}건 (최소 ${min})"
exit 0
