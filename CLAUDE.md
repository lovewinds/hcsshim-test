# vmrunner — HCS 기반 Linux VM 실행기

Windows의 `vmcompute.dll` HCS API를 직접 바인딩하여 경량 Linux VM을 부팅하고,
시리얼 콘솔을 통해 명령을 실행하는 Go 프로그램.

---

## 프로젝트 구조

```
/mnt/c/source/hcsshim/
├── go.mod                                  # module: github.com/microsoft/hcsshim/vmrunner
├── vm-image/                               # VM 이미지 (변경 금지)
│   ├── vmlinuz                             # Linux 커널
│   ├── initrd                              # 초기 램디스크
│   └── rootfs.vhdx                         # 루트 파일시스템
├── cmd/vmrunner/main.go                    # CLI 진입점 (서브커맨드 라우팅)
└── internal/
    ├── vmcompute/
    │   ├── types.go                        # HcsSystem, HcsProcess, HcsProcessInformation
    │   └── vmcompute.go                    # vmcompute.dll syscall 바인딩
    ├── config/config.go                    # HCS schema2 JSON 빌더
    └── vm/
        ├── vm.go                           # VM 생명주기 (Start/Shutdown/Attach/Exec 등)
        └── process.go                      # 시리얼 콘솔 + GCS 프로세스 실행
```

---

## 빌드

```bash
# WSL2에서 크로스 컴파일 (권장)
GOOS=windows GOARCH=amd64 go build -o /tmp/vmrunner.exe ./cmd/vmrunner
# vmrunner.exe가 Windows에서 실행 중이면 WSL 마운트 경로에 쓰기 권한이 없으므로 /tmp 사용
```

모든 소스 파일에 `//go:build windows` 태그가 있으므로 Linux에서도 크로스 컴파일 가능.

---

## 실행 (Windows 관리자 권한 필요)

```powershell
vmrunner run                        # VM 시작, 백그라운드 유지
vmrunner run -i                     # VM 시작, 인터랙티브 셸 (종료 시 VM shutdown)
vmrunner run -memory 4096 -cpu 4 -i # 4GB/4코어 인터랙티브
vmrunner run -debug -i              # HCS JSON 출력 후 시작
vmrunner run -id my-vm -i           # VM ID 지정
vmrunner exec ls -la                # 시리얼 콘솔로 명령 실행 (VM 없으면 자동 시작)
vmrunner exec -id my-vm cat /etc/os-release
vmrunner list                       # 실행 중인 VM 목록
vmrunner attach vmrunner-vm         # 실행 중인 VM 콘솔에 연결
vmrunner stop   vmrunner-vm         # Graceful shutdown
vmrunner kill   vmrunner-vm         # 강제 종료
```

공통 숨김 플래그: `-trace` (stdin→pipe 바이트 로그 + HCS JSON 출력)

---

## HCS API — 핵심 발견 사항 (디버깅 이력)

### vmcompute.dll API 버전

이 시스템의 `vmcompute.dll`은 **구 API (pre-computecore.h)** 를 사용한다.

| 함수 | 구 API 시그니처 |
|---|---|
| `HcsCreateComputeSystem` | `(Id, Configuration, Identity HANDLE, *System, *Result) HRESULT` |
| `HcsOpenComputeSystem` | `(Id, *System, *Result) HRESULT` |
| `HcsStartComputeSystem` | `(System, Options, *Result) HRESULT` |
| `HcsShutdownComputeSystem` | `(System, Options, *Result) HRESULT` |
| `HcsTerminateComputeSystem` | `(System, Options, *Result) HRESULT` |
| `HcsCloseComputeSystem` | `(System) HRESULT` |
| `HcsCreateProcess` | `(System, Params, *ProcessInfo, *Process, *Result) HRESULT` |

> **주의**: 신 API (computecore.h) 시그니처와 파라미터 순서가 다름.
> 신 API의 `HcsOpenComputeSystem(Id, DWORD AccessMask, *System)`처럼
> 2번째 파라미터가 DWORD인 버전을 사용하면 **ACCESS_VIOLATION** 크래시 발생.
> (크래시 증거: `rbx=0x1f0003` 주소로 쓰기 시도)

### HCS_OPERATION_PENDING (0xC0370103)

`HcsCreateComputeSystem`, `HcsStartComputeSystem` 등이 `0xC0370103`을 반환하는 경우,
이는 **에러가 아니라** 비동기 완료 대기 신호다 (`HCS_OPERATION_PENDING`).

- system 핸들은 유효하게 설정된 상태
- `HcsRegisterComputeSystemCallback`으로 완료 이벤트를 기다려야 함
- 구현: `waitForSystemNotification()` 함수 — `syscall.NewCallback` 트램폴린 사용
  - Windows amd64는 단일 호출 규약이라 `syscall.NewCallback`이 정상 동작

### 완료 알림 타입

| 상수 | 값 | 용도 |
|---|---|---|
| `hcsNotificationSystemExited` | `0x1` | Shutdown/Terminate 대기 |
| `hcsNotificationSystemCreateCompleted` | `0x2` | Create 대기 |
| `hcsNotificationSystemStartCompleted` | `0x3` | Start 대기 |

### Identity 파라미터

`HcsCreateComputeSystem`의 3번째 파라미터 `Identity`는 `SECURITY_DESCRIPTOR HANDLE`.
→ **항상 `0` (NULL)** 을 넘겨야 함. 문자열 포인터나 다른 값을 넘기면 오류.

---

## HCS JSON 설정 구조

```json
{
  "Owner": "vmrunner",
  "SchemaVersion": {"Major": 2, "Minor": 1},
  "VirtualMachine": {
    "Chipset": {
      "LinuxKernelDirect": {
        "KernelFilePath": "C:\\source\\hcsshim\\vm-image\\vmlinuz",
        "InitRdPath":     "C:\\source\\hcsshim\\vm-image\\initrd",
        "KernelCmdLine":  "console=ttyS0 root=/dev/sda rw init=/sbin/init"
      }
    },
    "ComputeTopology": {
      "Memory":    {"SizeInMB": 2048},
      "Processor": {"Count": 2}
    },
    "Devices": {
      "Scsi": {
        "0": {"Attachments": {"0": {"Type": "VirtualDisk", "Path": "C:\\source\\hcsshim\\vm-image\\rootfs.vhdx"}}}
      },
      "ComPorts": {
        "0": {"NamedPipe": "\\\\.\\pipe\\vmrunner-vm-console"}
      }
    }
  }
}
```

- `GuestConnection` 필드는 시리얼 콘솔 전용 구성에서 **제거** (불필요)
- SCSI LUN 0 (`root=/dev/sda`) = SCSI 컨트롤러 0, 어태치먼트 0

---

## 에러 진단

### FormatMessage 통합

`hresultError(hr, detail)` 함수가 `FormatMessage`로 HRESULT 설명을 자동 포함:

```
HRESULT 0xC0370103 (설명): HCS 상세 메시지
```

### 과거 에러 이력

| 에러 | 원인 | 해결 |
|---|---|---|
| `0xC0370103` (최초) | Identity 파라미터에 문자열 포인터 전달 | NULL(0) 전달 |
| ACCESS_VIOLATION at `0x1f0003` | `HcsOpenComputeSystem`에 신 API 시그니처 사용 | 구 API로 복원 |
| `0xC0370103` (지속) | PENDING을 에러로 처리 | PENDING 감지 후 콜백 대기 |

---

## 시리얼 콘솔 입력 — 핵심 발견 사항

### 문제: ConPTY + SetConsoleMode + ReadConsole 충돌

Go의 `os.Stdin.Read()`는 내부적으로 Win32 `ReadConsole`을 호출한다.
Windows Terminal(ConPTY) 환경에서 `SetConsoleMode`를 **단 한 번이라도** 호출하면
`ReadConsole`이 Enter 이후에도 영원히 블록되는 상태가 된다.
제거하는 플래그 종류(LINE_INPUT, ECHO_INPUT 등)와 무관하게 동일하게 발생한다.

### 해결: VTI 모드 + ReadFile 직접 호출

`SetConsoleMode`를 호출한 후 **`ReadConsole`(`os.Stdin.Read()`) 대신
`ReadFile`을 콘솔 HANDLE에 직접 사용**하면 이 문제를 우회할 수 있다.

`prepareRawConsole()` 함수 (`internal/vm/process.go`):
- `GetConsoleMode`로 현재 모드 저장
- `SetConsoleMode`로 VTI 모드 설정:
  - 제거: `ENABLE_LINE_INPUT` (0x0002) — 줄버퍼링 해제, 키 즉시 전달
  - 제거: `ENABLE_ECHO_INPUT` (0x0004) — 로컬 에코 제거 (이중 에코 방지)
  - 추가: `ENABLE_VIRTUAL_TERMINAL_INPUT` (0x0200) — ReadFile 전용 VT 시퀀스 모드
  - 유지: `ENABLE_PROCESSED_INPUT` (0x0001) — Ctrl+C → SIGINT 유지
- stdin이 콘솔이 아닌 경우(리다이렉트) `GetConsoleMode` 실패 → 자동 폴백
- 종료 시 `defer restore()`로 원래 모드 복원

`stdinToPipe()` 함수:
- `isConsole=true`: `syscall.ReadFile(stdinH, ...)` 직접 사용 (ReadConsole 우회)
- `isConsole=false`: `os.Stdin.Read()` 기존 방식 유지 (리다이렉트 케이스)
- `\r` → `\n` 변환 유지 (VTI 모드에서 Enter = `\r`)

### 효과

| 항목 | 결과 |
|---|---|
| 이중 에코 | 해결 — 로컬 에코 제거, VM echo만 표시 |
| 문자 단위 즉시 전달 | 해결 — LINE_INPUT 제거 |
| 방향키, Tab 등 | 동작 — VT 시퀀스로 전달, VM shell line editing 작동 |
| Ctrl+C | 동작 유지 — PROCESSED_INPUT 유지, SIGINT 생성 |
| stdin 리다이렉트 | 자동 폴백, 기존 동작 유지 |

---

## 프로세스 실행 전략

1. **GCS 경로** (`HcsCreateProcess`): initrd에 GCS 에이전트가 있어야 동작.
   stdio 핸들이 0이면 즉시 시리얼 콘솔로 폴백.
2. **시리얼 콘솔** (`\\.\pipe\{vmID}-console`):
   - 100ms 간격으로 최대 30초 재시도 (VM 시작 후 pipe 생성됨)
   - `InteractiveShell`: stdin↔pipe 양방향, VTI raw 모드
   - `RunCommand`: 명령 전송 → 프롬프트(`#`/`$`) 감지로 완료 판단

---

## 타임아웃 설정

| 작업 | 타임아웃 |
|---|---|
| VM Create 완료 대기 | 60초 |
| VM Start (커널 부팅) 대기 | 120초 |
| Graceful Shutdown 대기 | 30초 |
| Terminate 대기 | 10초 |
| Serial console 파이프 연결 | 30초 |
