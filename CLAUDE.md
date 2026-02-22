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
├── cmd/vmrunner/main.go                    # CLI 진입점
└── internal/
    ├── vmcompute/
    │   ├── types.go                        # HcsSystem, HcsProcess, HcsProcessInformation
    │   └── vmcompute.go                    # vmcompute.dll syscall 바인딩
    ├── config/config.go                    # HCS schema2 JSON 빌더
    └── vm/
        ├── vm.go                           # VM 생명주기 (create/start/shutdown)
        └── process.go                      # 시리얼 콘솔 + GCS 프로세스 실행
```

---

## 빌드

```bash
# WSL2에서 크로스 컴파일 (권장)
cd /mnt/c/source/hcsshim
GOOS=windows GOARCH=amd64 go build -o vmrunner.exe ./cmd/vmrunner
```

모든 소스 파일에 `//go:build windows` 태그가 있으므로 Linux에서도 크로스 컴파일 가능.

---

## 실행 (Windows 관리자 권한 필요)

```powershell
.\vmrunner.exe -i                          # 인터랙티브 셸
.\vmrunner.exe ls /                        # 명령 실행 (시리얼 콘솔 폴백)
.\vmrunner.exe -memory 4096 -cpu 4 -i      # 4GB/4코어
.\vmrunner.exe -debug -i                   # HCS JSON 출력 후 시작
.\vmrunner.exe -id my-vm -i               # VM ID 지정
```

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

## 프로세스 실행 전략

1. **GCS 경로** (`HcsCreateProcess`): initrd에 GCS 에이전트가 있어야 동작.
   stdio 핸들이 0이면 즉시 시리얼 콘솔로 폴백.
2. **시리얼 콘솔 폴백** (`\\.\pipe\{vmID}-console`):
   - 100ms 간격으로 최대 30초 재시도 (VM 시작 후 pipe 생성됨)
   - 인터랙티브: stdin↔pipe 양방향 복사
   - 명령 실행: 명령 전송 → 프롬프트(`#`/`$`) 감지로 완료 판단

---

## 타임아웃 설정

| 작업 | 타임아웃 |
|---|---|
| VM Create 완료 대기 | 60초 |
| VM Start (커널 부팅) 대기 | 120초 |
| Graceful Shutdown 대기 | 30초 |
| Terminate 대기 | 10초 |
| Serial console 파이프 연결 | 30초 |
