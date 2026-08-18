# T8 픽스처 녹화 목록

녹화일: 2026-08-18

도구:

- claude-code 2.1.233
- codex-cli 0.147.0

| 파일 | scenario | coverage | 검증 포인트 확인 |
|---|---|---|---|
| claude-code/01-simple-text.ndjson | simple-text | normal | tool_use 0 |
| claude-code/02-single-tool.ndjson | single-tool | — | tool_use 1, tool_result 1 |
| claude-code/03-multi-tool.ndjson | multi-tool | multi-tool | tool_use 2, tool_result 2 |
| claude-code/04-edit-file.ndjson | edit-file | — | Edit 1, 최종 내용 world |
| claude-code/05-approval-denied.ndjson | approval-denied | approval-denied | permission_denials 1, 외부 파일 미생성 |
| claude-code/06-tool-error.ndjson | tool-error | error | is_error 1, 파일 미생성 |
| claude-code/07-command-fail.ndjson | command-fail | error | 종료 코드 42 |
| claude-code/08-interrupted.ndjson | interrupted | interrupted | error_during_execution, 완료 전 Ctrl-C |
| codex/01-simple-text.ndjson | simple-text | normal | command_execution 0 |
| codex/02-single-tool.ndjson | single-tool | — | command_execution 1 |
| codex/03-multi-tool.ndjson | multi-tool | multi-tool | command_execution 2 |
| codex/04-edit-file.ndjson | edit-file | — | file_change 1, 최종 내용 world |
| codex/06-tool-error.ndjson | tool-error | error | command exit non-zero |
| codex/07-command-fail.ndjson | command-fail | error | 종료 코드 42 |
| codex/08-interrupted.ndjson | interrupted | interrupted | command_execution in_progress에서 Ctrl-C |

## coverage 충족

- normal: claude-code/01, codex/01
- multi-tool: claude-code/03, codex/03
- approval-denied: claude-code/05
- error: claude-code/06, claude-code/07, codex/06, codex/07
- interrupted: claude-code/08, codex/08

## 계수

- 실제 NDJSON 녹화: 15건
- meta-only skip: 1건
- 총 meta: 16건

## meta-only skip

- codex/05-approval-denied.meta.txt
  - `codex exec --json`에서 승인 UI가 표면화되지 않음
  - `/tmp` 쓰기가 허용되어 승인 거부 픽스처로 사용할 수 없었음
  - 유효 녹화 15건 계수에서 제외
