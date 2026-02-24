# 시리얼 콘솔 입력 디버깅 — 발견 사항 및 해결 기록

## 결론 (해결 완료)

VM 부팅 후 시리얼 콘솔에 명령을 입력해도 VM이 응답하지 않거나,
입력한 문자가 두 번 표시되는 문제.

**최종 해결책**: `prepareRawConsole()` + `syscall.ReadFile` 직접 사용.
- `SetConsoleMode`로 VTI raw 모드 설정 (로컬 에코 및 줄버퍼링 제거)
- `os.Stdin.Read()`(= ReadConsole 경로) 대신 `ReadFile`을 콘솔 HANDLE에 직접 사용
- ReadConsole 경로를 우회하여 ConPTY 충돌 회피

---

## 확인된 사실

### VM 출력 (pipe read, 호스트 → stdout)

- **정상 동작** 처음부터 — 부팅 메시지, 프롬프트 모두 표시됨

### VM 입력 (stdin → pipe write, 호스트 → VM)

- 파이프는 **양방향(duplex)** 으로 확인됨 (HCS가 COM 포트를 DUPLEX로 생성)
- `WriteFile`이 `written=N, err=<nil>`로 성공하면 VM이 명령을 수신하고 실행함
- VTI 모드에서 Enter 키 = `\r` → 코드에서 `\n`으로 변환하여 전송

### 로컬 에코와 VM 에코

- Cooked mode에서는 Windows Terminal이 타이핑 즉시 로컬 에코 표시
- Linux ttyS0의 ECHO(getty가 초기화한 경우)가 파이프로 되돌려보내면 이중 에코 발생
- VTI raw 모드(`ENABLE_ECHO_INPUT` 제거) + ReadFile로 로컬 에코를 제거하여 해결

---

## 근본 원인 상세 분석

### ConPTY + SetConsoleMode + ReadConsole 삼각 충돌

Go의 Windows `os.Stdin.Read()`는 내부적으로 Win32 `ReadConsole`을 호출한다.
Windows Terminal은 ConPTY(Pseudo Console)를 사용해 자식 프로세스의 콘솔을 가상화한다.

`SetConsoleMode`를 호출하면 ConPTY 레이어가 **input delivery 방식**을 변경한다.
이 변경이 Go의 `ReadConsole` 호출과 충돌하여 ReadConsole이 Enter 이후에도
반환하지 않는 상태가 된다.

제거하는 플래그와 무관하게 동일하게 발생:
- `ENABLE_LINE_INPUT | ENABLE_ECHO_INPUT` 제거 → stdin 완전 차단
- `ENABLE_ECHO_INPUT`만 제거 → 동일하게 stdin 차단
- 즉, **SetConsoleMode 호출 자체**가 ReadConsole을 파괴

### 핵심 인사이트

`SetConsoleMode` + `ReadConsole` 조합이 문제.
`SetConsoleMode` 후 **`ReadFile`** 을 사용하면 이 충돌이 발생하지 않는다.
MSDN도 `ENABLE_VIRTUAL_TERMINAL_INPUT`은 `ReadFile` 전용임을 명시하고 있다.

---

## 시도 이력

### 시도 1: ENABLE_VIRTUAL_TERMINAL_INPUT + ReadConsole

```go
newIn := (oldIn &^ (enableLineInput | enableEchoInput)) | enableVirtualTerminalInput
// + os.Stdin.Read() 사용
```

**결과**: 화면에 아무것도 표시되지 않고 응답 없음
**원인**: MSDN 명시 — `ENABLE_VIRTUAL_TERMINAL_INPUT`은 `ReadFile` 전용. `ReadConsole`과 함께 사용하면 동작 미정의.

---

### 시도 2: Raw mode (LINE_INPUT + ECHO 제거) + ReadConsole

```go
newIn := oldIn &^ (enableLineInput | enableEchoInput)
// + os.Stdin.Read() 사용
```

**결과**: 아무것도 표시 안 됨, stdin 완전 차단
**원인**: `SetConsoleMode` 호출 자체가 Go ReadConsole을 파괴 (ConPTY 충돌)

---

### 시도 3: Cooked mode + `\r\n` → `\r` 필터링

**결과**: Enter 후 VM 무반응
**원인**: `\r`은 Linux tty canonical mode의 line terminator가 아님. 올바른 변환은 `\r` → `\n`.

---

### 시도 4: Cooked mode + `\r\n` → `\n` 변환

**결과**: stdin 읽기는 동작했으나 VM 무반응
**원인**: 당시 overlapped I/O 미적용 상태. 파이프 쓰기 성공 여부 미확인.

---

### 시도 5: Overlapped I/O 적용

**변경**: 단일 핸들에 `FILE_FLAG_OVERLAPPED`, ReadFile/WriteFile 모두 overlapped.
**결과**: stdin 읽기는 동작. VM 응답 여부 여전히 미확인.
**의의**: 동일 핸들 동시 R/W 직렬화(데드락) 방지.

---

### 시도 6-7: Raw mode 변형 (ECHO만 제거 등)

**결과**: stdin 여전히 차단 (`[vmrunner] stdin read:` 로그 미출력)
**원인**: SetConsoleMode 호출 자체(어떤 플래그든)가 ConPTY와 충돌. LINE_INPUT 유지 여부와 무관.

---

### 시도 8 (임시 해결): SetConsoleMode 완전 제거

**변경**: `InteractiveShell()`에서 `SetConsoleMode` 계열 함수 호출 제거. Cooked mode 사용.
**결과**: stdin 동작, VM 명령 응답. 그러나 로컬 에코 + VM echo로 이중 표시 발생.

---

### 최종 해결: VTI 모드 + ReadFile 직접 사용

**변경**: `prepareRawConsole()` 함수 추가. VTI 모드 설정 후 `syscall.ReadFile`로 읽기.

```go
// SetConsoleMode: LINE_INPUT | ECHO_INPUT 제거, VTI 추가, PROCESSED_INPUT 유지
newMode := (old &^ (enableLineInput | enableEchoInput)) | enableVirtualTermInput
procSetConsoleMode.Call(uintptr(stdinH), uintptr(newMode))

// ReadConsole 우회: ReadFile 직접 사용
readErr = syscall.ReadFile(stdinH, inBuf, &n, nil)
```

**결과**:
```
root@(none):/# ls        ← 글자 한 번만 표시 (이중 에코 없음)
bin  boot  dev  etc  home  lib  ...
root@(none):/#
```

---

## 현재 구현 상태

```
InteractiveShell(pipeName)
  └─ openOverlappedPipeWithRetry   # FILE_FLAG_OVERLAPPED, 단일 핸들
  ├─ pipeToStdout(h)               # overlapped ReadFile → os.Stdout
  └─ stdinToPipe(h)
       ├─ prepareRawConsole()      # VTI raw 모드, defer restore()
       ├─ ReadFile(stdinH)         # 콘솔 직접 읽기 (ReadConsole 우회)
       └─ CR→LF 변환 → overlapped WriteFile(h)
```

**입력 모드**: VTI raw (콘솔인 경우) / Cooked 폴백 (리다이렉트인 경우)
- `ENABLE_ECHO_INPUT` 비활성 → 로컬 에코 없음 (VM echo만 표시)
- `ENABLE_LINE_INPUT` 비활성 → 키 단위 즉시 전달
- `ENABLE_VIRTUAL_TERMINAL_INPUT` 활성 → 방향키 등 VT 시퀀스 전달
- `ENABLE_PROCESSED_INPUT` 활성 → Ctrl+C → SIGINT 유지

**CR→LF 변환 로직**:
- `\r` → `\n` (VTI 모드에서 Enter = `\r`)
- `\r\n` → `\n` (CR 이후 LF는 중복 제거)
- 나머지 바이트는 그대로 전달

---

## 사용법

```powershell
# 인터랙티브 셸
.\vmrunner.exe run -i

# stdin→pipe 트레이스 (디버깅)
.\vmrunner.exe run -i -trace

# 실행 중인 VM에 연결
.\vmrunner.exe attach vmrunner-vm
```
