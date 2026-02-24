# 시리얼 콘솔 입력 디버깅 — 발견 사항 및 해결 기록

## 결론 (해결 완료, 2026-02-24)

VM 부팅 후 시리얼 콘솔에 명령을 입력해도 VM이 응답하지 않는 문제.

**근본 원인**: Windows Terminal(ConPTY) 환경에서 `SetConsoleMode`를 단 한 번이라도
호출하면 Go의 `os.Stdin.Read()`가 Enter 이후에도 영원히 블록된다.

**해결책**: `SetConsoleMode`를 호출하지 않는다. Cooked mode(기본값) 그대로 사용.
`os.Stdin.Read()`가 Enter 후 한 줄씩 반환하는 방식으로 정상 동작한다.

---

## 확인된 사실

### VM 출력 (pipe read, 호스트 → stdout)

- **정상 동작** 처음부터 — 부팅 메시지, 프롬프트 모두 표시됨

### VM 입력 (stdin → pipe write, 호스트 → VM)

- 파이프는 **양방향(duplex)** 으로 확인됨 (HCS가 COM 포트를 DUPLEX로 생성)
- `WriteFile`이 `written=N, err=<nil>`로 성공하면 VM이 명령을 수신하고 실행함
- Enter 입력 시 Windows 콘솔은 `\r\n`을 반환 → 코드에서 `\n`으로 변환하여 전송

### Windows 로컬 에코

- Cooked mode에서 타이핑 시 Windows 콘솔/Windows Terminal이 로컬로 에코
- 이 에코는 VM echo와 **무관**하며 `SetConsoleMode`로 제어할 수 없음
  (Windows Terminal이 VT 레이어에서 독립적으로 처리)

---

## 근본 원인 상세 분석

### SetConsoleMode가 os.Stdin.Read()를 파괴하는 이유

Go의 Windows `os.Stdin.Read()`는 내부적으로 Win32 `ReadConsole`을 호출한다.
Windows Terminal은 ConPTY(Pseudo Console)를 사용해 자식 프로세스의 콘솔을 가상화한다.

`SetConsoleMode`를 호출하면 ConPTY 레이어가 **input delivery 방식**을 변경한다.
이 변경이 Go의 `ReadConsole` 호출과 충돌하여 ReadConsole이 Enter 이후에도
반환하지 않는 상태가 된다.

제거하는 플래그와 무관하게 동일하게 발생:
- `ENABLE_LINE_INPUT | ENABLE_ECHO_INPUT` 제거 → stdin 완전 차단
- `ENABLE_ECHO_INPUT`만 제거 → 동일하게 stdin 차단
- 즉, **SetConsoleMode 호출 자체**가 문제

### 핵심 증거

| 조건 | stdin 읽기 결과 |
|---|---|
| SetConsoleMode 미호출 (cooked mode) | `os.Stdin.Read()` 정상 반환 ✓ |
| `ENABLE_LINE_INPUT\|ECHO` 제거 | stdin 완전 차단 ✗ |
| `ENABLE_ECHO_INPUT`만 제거 | stdin 완전 차단 ✗ |

진단 근거: `-trace` 플래그로 `[vmrunner] trace: stdin→pipe` 로그가 표시되는지 확인.

---

## 시도 이력 (실패한 접근들)

### 시도 1: ENABLE_VIRTUAL_TERMINAL_INPUT 추가

```go
newIn := (oldIn &^ (enableLineInput | enableEchoInput)) | enableVirtualTerminalInput
```

**결과**: 화면에 아무것도 표시되지 않고 응답 없음
**원인**: MSDN 명시 — `ENABLE_VIRTUAL_TERMINAL_INPUT`은 `ReadFile` 전용.
`ReadConsole`과 함께 사용하면 동작 미정의.

---

### 시도 2: Raw mode (LINE_INPUT + ECHO 제거)

```go
newIn := oldIn &^ (enableLineInput | enableEchoInput)
```

**결과**: 아무것도 표시 안 됨, stdin 완전 차단
**원인**: `SetConsoleMode` 호출 자체가 Go ReadConsole을 파괴 (ConPTY 충돌)

---

### 시도 3: Cooked mode + `\r\n` → `\r` 필터링

**결과**: Enter 후 VM 무반응
**원인**: `\r`은 Linux tty canonical mode의 line terminator가 아님.
올바른 변환은 `\r` → `\n`.

---

### 시도 4: Cooked mode + `\r\n` → `\n` 변환

**결과**: stdin 읽기는 동작했으나 VM 무반응
**원인**: 당시 overlapped I/O 미적용 상태. 파이프 쓰기 성공 여부 미확인.
(별도 진단 로그 없이 결론 내림 → 불완전한 디버깅)

---

### 시도 5: Overlapped I/O 적용

**변경**: 단일 핸들에 `FILE_FLAG_OVERLAPPED`, ReadFile/WriteFile 모두 overlapped.

**결과**: stdin 읽기는 동작. VM 응답 여부 여전히 미확인.
**의의**: 동일 핸들 동시 R/W 직렬화(데드락) 방지.

---

### 시도 6: setConsoleRaw (시도 2와 동일 마스크, 코드 리팩터링)

**결과**: stdin 완전 차단 (시도 2와 동일)

---

### 시도 7: disableLocalEcho (ECHO만 제거, LINE_INPUT 유지)

**이론**: Go의 `ReadConsole`은 `ENABLE_LINE_INPUT` 없으면 동작 미정의.
LINE_INPUT 유지 시 정상 동작할 것이라 예상.

**결과**: stdin 여전히 차단. `[vmrunner] stdin read:` 로그 미출력.
**원인**: SetConsoleMode 호출 자체(어떤 플래그든)가 ConPTY와 충돌.
LINE_INPUT 유지 여부와 무관.

---

### 해결: SetConsoleMode 호출 완전 제거

**변경**: `InteractiveShell()`에서 `SetConsoleMode` 계열 함수 호출 제거.
Cooked mode 그대로 사용. 진단 로그 추가.

**결과**:
```
root@(none):/# ls
[vmrunner] trace: stdin→pipe "ls\n" written=3
ls
bin  boot  dev  etc  home  lib  ...
root@(none):/#
```

---

## 현재 구현 상태

```
InteractiveShell(pipeName)
  └─ openOverlappedPipeWithRetry   # FILE_FLAG_OVERLAPPED, 단일 핸들
  ├─ pipeToStdout(h)               # overlapped ReadFile → os.Stdout
  └─ stdinToPipe(h)                # os.Stdin.Read (cooked) → CR→LF → overlapped WriteFile
                                   # vm.Trace=true 시 stdin→pipe 바이트 로그 출력
```

**입력 모드**: Cooked (SetConsoleMode 미호출)
- `ENABLE_LINE_INPUT` 활성 → Enter 후 한 줄 전체 반환
- 로컬 에코 활성 → 타이핑 시 Windows Terminal이 표시 (VM echo와 별개)
- Enter 입력 시 `\r\n` 수신 → `\n` 변환 후 VM으로 전송

**CR→LF 변환 로직**:
- `\r` → `\n` (CR을 LF로 교체)
- `\r\n` → `\n` (CR 이후 LF는 중복이므로 제거)
- 나머지 바이트는 그대로 전달

---

## 사용법

```powershell
# 인터랙티브 셸
.\vmrunner.exe -i

# stdin→pipe 트레이스 (디버깅)
.\vmrunner.exe -i -trace

# HCS JSON 설정 출력 후 시작
.\vmrunner.exe -i -debug
```

---

## 미해결 / 향후 과제

- **로컬 에코 이중 표시**: VM의 ttyS0 ECHO가 활성화되어 있으면 입력한 문자가
  두 번 표시될 수 있음 (로컬 에코 1회 + VM echo 1회). 현재 `init=/bin/bash`
  환경에서는 ttyS0 ECHO 상태가 불확실하며 실제로는 로컬 에코만 보임.

- **init=/sbin/init**: 현재 `init=/bin/bash`로 테스트 중. 프로덕션 전환 시
  getty가 ttyS0를 올바르게 초기화(ICRNL, ICANON, ECHO)하는 `init=/sbin/init`
  사용 권장.
