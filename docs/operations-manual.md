# oss-sync 运维操作手册

面向运维人员，说明 **配置文件写法、核心命令、日常操作流程**。本文默认已经拿到可执行文件和对象存储访问凭证。

## 1. 适用场景

- 需要把一个对象存储目录同步到另一个对象存储目录
- 需要长期运行增量同步任务
- 需要查看同步进度、失败文件和失败原因
- 需要在上线前验证 source / dest 配置是否可用

## 2. 部署前准备

### 2.1 必备信息

- 源端：`provider`、`endpoint`、`access_key_id`、`access_key_secret`、`bucket`
- 目标端：`provider`、`endpoint`、`access_key_id`、`access_key_secret`、`bucket`
- 同步范围：源目录前缀、目标目录前缀

### 2.2 支持的 provider

| provider | 用途 |
|---|---|
| `oss` | 阿里云 OSS，既可做 source，也可做 dest |
| `s3` | 任意 S3 兼容存储，既可做 source，也可做 dest |
| `obs` | 华为云 OBS，仅可做 dest |

### 2.3 建议目录

```text
/opt/oss-sync/
  oss-sync                 # 二进制
  config.yaml              # 运行配置
  sync.db                  # SQLite 状态库
  logs/                    # 外部日志目录（可选）
```

`sync.db` 用于保存同步状态、失败原因、最近 session 信息，**不要随意删除**。

### 2.4 Windows PowerShell 使用说明

如果在 Windows 上本地运行源码，请使用下面的方式：

```powershell
go build -o oss-sync.exe .\cmd
.\oss-sync.exe test -c config.yaml
.\oss-sync.exe sync -c config.yaml
```

如果想直接用 `go run`，建议写成：

```powershell
go run .\cmd\main.go sync -c config.yaml
```

注意：

- 不要在仓库根目录直接执行 `go build -o xxx.exe`
- 根目录没有 Go 入口文件，因此会出现 `no Go files in D:\code\oss-sync`
- 入口在 `.\cmd` 目录下

如果出现：

```text
The specified executable is not a valid application for this OS platform
```

通常说明当前 `oss-sync.exe` 不是 Windows 二进制，而是之前在设置了 `GOOS=linux` 后编出来的 Linux 版本。可执行：

```powershell
Remove-Item Env:GOOS -ErrorAction SilentlyContinue
Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue
go build -o oss-sync.exe .\cmd
```
## 3. 配置文件说明

### 3.1 最小可用示例

```yaml
source:
  provider: "oss"
  endpoint: "https://oss-cn-hangzhou.aliyuncs.com"
  access_key_id: "YOUR_SOURCE_AK"
  access_key_secret: "YOUR_SOURCE_SK"
  bucket: "source-bucket"
  prefix: "handao-dev/"
  insecure_skip_verify: false

dest:
  provider: "oss"
  endpoint: "https://oss-cn-hangzhou.aliyuncs.com"
  access_key_id: "YOUR_DEST_AK"
  access_key_secret: "YOUR_DEST_SK"
  bucket: "dest-bucket"
  prefix: "handao-dev-backup/"
  visibility: "source"
  insecure_skip_verify: false

sync:
  mode: "incremental"
  concurrency: 10
  rate_limit_mbps: 0
  page_size: 1000
  retry_count: 3
  db_path: "./sync.db"
```

### 3.2 OSS -> OBS 配置示例

当目标端是华为云 OBS 时，可参考：
```yaml
source:
  provider: "oss"
  endpoint: "https://oss-cn-hangzhou.aliyuncs.com"
  access_key_id: "YOUR_OSS_AK"
  access_key_secret: "YOUR_OSS_SK"
  bucket: "source-bucket"
  prefix: "images/raw/"

dest:
  provider: "obs"
  endpoint: "https://obs.cn-north-4.myhuaweicloud.com"
  access_key_id: "YOUR_OBS_AK"
  access_key_secret: "YOUR_OBS_SK"
  bucket: "dest-bucket"
  prefix: "backup/2026/"
  visibility: "source"

sync:
  mode: "incremental"
  concurrency: 10
  rate_limit_mbps: 50
  page_size: 1000
  retry_count: 3
  db_path: "./sync.db"
```

适用于：源端在阿里云 OSS，目标端在华为云 OBS 的迁移或备份场景。

注意：

- `dest.provider` 必须是 `obs`
- OBS 目前仅支持作为目标端使用

### 3.3 目录映射语义

如果配置：

- `source.prefix: handao-dev/`
- `dest.prefix: handao-dev-backup/`

那么：

- `handao-dev/a.txt -> handao-dev-backup/a.txt`
- `handao-dev/dir/b.txt -> handao-dev-backup/dir/b.txt`

也就是说，程序会保留 source prefix 后面的**相对路径**，再拼接到 dest prefix。

### 3.4 多组目录映射

当一个配置文件里需要同步多组目录时，使用 `sync.mappings`：

```yaml
source:
  provider: "oss"
  endpoint: "https://oss-cn-hangzhou.aliyuncs.com"
  access_key_id: "YOUR_SOURCE_AK"
  access_key_secret: "YOUR_SOURCE_SK"
  bucket: "source-bucket"

dest:
  provider: "oss"
  endpoint: "https://oss-cn-hangzhou.aliyuncs.com"
  access_key_id: "YOUR_DEST_AK"
  access_key_secret: "YOUR_DEST_SK"
  bucket: "dest-bucket"

sync:
  mode: "incremental"
  concurrency: 10
  rate_limit_mbps: 0
  page_size: 1000
  retry_count: 3
  db_path: "./sync.db"
  mappings:
    - source_prefix: "images/raw/"
      dest_prefix: "backup/raw/"
    - source_prefix: "images/thumbs/"
      dest_prefix: "backup/thumbs/"
```

说明：

- 配置了 `sync.mappings` 后，会优先按映射列表执行
- 每个映射内部仍使用 `sync.concurrency` 并发处理
- 每个映射有独立 scope，状态统计会自动聚合显示

### 3.5 关键字段说明

| 字段 | 说明 | 运维建议 |
|---|---|---|
| `source.provider` | 源端类型：`oss` / `s3` | MinIO 等 S3 兼容存储使用 `s3` |
| `source.endpoint` | 源端访问地址 | 建议写完整协议，如 `https://...` |
| `source.bucket` | 源 bucket 名称 | 需确认账号有读权限 |
| `source.prefix` | 源端同步目录 | 留空表示整桶 |
| `source.force_path_style` | S3 path style 开关 | MinIO 通常设为 `true` |
| `source.insecure_skip_verify` | 是否跳过 TLS 证书校验 | 私有 CA / 自签证书时使用 |
| `dest.provider` | 目标端类型：`obs` / `oss` / `s3` | OBS 只能做目标端 |
| `dest.endpoint` | 目标端访问地址 | 建议写完整协议 |
| `dest.bucket` | 目标 bucket 名称 | 需确认账号有写权限 |
| `dest.prefix` | 目标写入目录 | 留空表示按原 key 写入 |
| `dest.visibility` | 目标对象可见性 | 留空使用目标端默认值，`source` 表示尽量与源对象 ACL 保持一致 |
| `dest.force_path_style` | S3 path style 开关 | 仅 `s3` provider 有效 |
| `dest.insecure_skip_verify` | 是否跳过 TLS 证书校验 | 私有 CA / 自签证书时使用 |
| `sync.mode` | `full` 或 `incremental` | 首次迁移建议 `full` |
| `sync.concurrency` | 并发 worker 数 | 带宽或后端限流敏感时适当调小 |
| `sync.rate_limit_mbps` | 总带宽限制，单位 MB/s | `0` 表示不限速 |
| `sync.page_size` | 每页列举对象数 | 默认 1000 通常够用 |
| `sync.retry_count` | 单文件失败自动重试次数 | 默认 3 |
| `sync.db_path` | SQLite 状态库路径 | 建议放在持久目录 |
| `sync.mappings` | 多组目录映射 | 与 `source.prefix` / `dest.prefix` 二选一优先使用 |

`dest.visibility` 支持范围：

- 所有目标端通用：`source`、`private`、`public-read`、`public-read-write`
- `obs` / `s3` 额外支持：`authenticated-read`、`bucket-owner-read`、`bucket-owner-full-control`
- `oss` 目标端仅支持：`private`、`public-read`、`public-read-write`

如果配置为 `source`，程序会读取源对象 ACL 并映射到目标端；该模式面向标准 canned ACL。若源对象使用的是无法识别的自定义 ACL，任务会报错，便于运维排查。

## 4. 核心命令

以下命令都支持 `-c <配置文件路径>`。

### 4.1 验证配置

```bash
./oss-sync test -c config.yaml
```

用途：

- 验证 source 能否正常列举
- 验证 dest 能否正常访问 bucket
- 不会写入测试文件

成功输出示例：

```text
[OK] source provider=oss bucket=source-bucket endpoint=https://oss-cn-hangzhou.aliyuncs.com
     prefix=handao-dev/
[OK] dest provider=oss bucket=dest-bucket endpoint=https://oss-cn-hangzhou.aliyuncs.com
     prefix=handao-dev-backup/
Configuration test passed.
```

### 4.2 执行同步

```bash
./oss-sync sync -c config.yaml
```

说明：

- 在交互式终端中默认进入 TUI
- 在非 TTY 环境中自动输出普通日志
- TUI 中按 `q` 可退出界面并取消当前同步

常见变体：

```bash
# 强制全量同步
./oss-sync sync -c config.yaml -m full

# 禁用 TUI，适合后台任务 / 重定向日志
./oss-sync sync -c config.yaml --no-tui
```

### 4.3 查看统计

```bash
./oss-sync stats -c config.yaml
```

常见变体：

```bash
# 强制实时 TUI
./oss-sync stats -c config.yaml --watch

# 输出一次快照
./oss-sync stats -c config.yaml --no-tui

# 设置刷新频率
./oss-sync stats -c config.yaml --interval 1s
```

重点指标说明：

| 指标 | 含义 |
|---|---|
| `Total files` | 全量累计文件数，包含历史记录 |
| `Synced files` | 全量累计成功文件数 |
| `Pending` | 当前仍待处理的文件数 |
| `Failed` | 当前失败文件数 |
| `Files/sec` | 本次启动后的平均文件同步速率 |
| `Bytes/sec` | 本次启动后的实时字节吞吐 |
| `Discover/sec` | 本次启动后的文件发现速率 |
| `Discovery` | 文件发现阶段状态 |

### 4.4 查看失败文件

```bash
./oss-sync failed -c config.yaml --limit 100
```

用途：

- 快速查看失败文件
- 查看失败原因
- 为补偿、重试、人工排查提供依据

### 4.5 一键打包并上传 Linux 版本

适用于 Windows 运维机：

```powershell
.\build-upload-linux.ps1 -ConfigPath config-handao-dev.yaml -RemoteDir oss-sync
```

默认行为：

- 交叉编译 `linux/amd64`
- 上传二进制
- 同时上传指定配置文件

### 4.6 一键打包 Windows / macOS / Linux

如果需要一次性产出三个平台的可执行文件，可执行：

```powershell
.\build-all-platforms.ps1
```

默认输出到 `dist\` 目录：

- `oss-sync-windows-amd64.exe`
- `oss-sync-linux-amd64`
- `oss-sync-darwin-amd64`

## 5. 推荐操作流程

### 场景一：首次上线

1. 准备配置文件
2. 执行 `./oss-sync test -c config.yaml`
3. 先用 `./oss-sync sync -c config.yaml -m full` 做首次全量同步
4. 用 `./oss-sync stats -c config.yaml --no-tui` 查看结果
5. 如有失败，用 `./oss-sync failed -c config.yaml --limit 100` 排查

### 场景二：日常增量同步

1. 保持配置中的 `sync.mode: incremental`
2. 执行 `./oss-sync test -c config.yaml`
3. 执行 `./oss-sync sync -c config.yaml --no-tui`
4. 定期用 `stats` 和 `failed` 检查运行结果

### 场景三：排查失败任务

1. 运行 `./oss-sync failed -c config.yaml --limit 100`
2. 根据错误信息确认是网络、权限、证书还是限流问题
3. 如需重新同步，可直接再次执行 `sync`
4. 工具会对单文件自动重试，默认 3 次；仍失败的会保留失败记录

## 6. 常见运维注意事项

### 6.1 关于数据库

- `sync.db` 记录同步状态和 session 历史
- 删除数据库会导致历史状态丢失
- 首次全量之后，后续增量同步依赖该库判断状态

### 6.2 关于证书

如果对象存储使用私有 CA 或自签证书，可配置：

```yaml
source:
  insecure_skip_verify: true

dest:
  insecure_skip_verify: true
```

仅在受控内网环境中使用该选项。

### 6.3 关于重试

```yaml
sync:
  retry_count: 3
```

说明：

- 下载或上传失败时会自动重试
- 只有重试耗尽后才会标记为失败

### 6.4 关于无头运行

如果放到 crontab、systemd 或其他任务调度器中，建议使用：

```bash
./oss-sync sync -c config.yaml --no-tui
```

这样更适合记录日志和后台运行。

### 6.5 关于会话保持

如果需要手工长时间运行同步任务，建议在 Linux 服务器上配合 `tmux`、`screen` 等窗口复用工具启动任务，避免因为 SSH 断开或终端关闭导致同步中断。

即使任务中途中断，后续重新执行 `sync` 也会基于 SQLite 状态库继续处理未完成对象，具备断点续传能力；但前提是 **保留原有 `sync.db`**，不要删除或替换该文件。

## 7. 故障排查

| 现象 | 常见原因 | 处理建议 |
|---|---|---|
| `test` 命令失败 | endpoint、bucket、AK/SK、证书有问题 | 先核对配置，再检查网络和权限 |
| `Files/sec` 很低 | 带宽限制、对象过大、后端限流 | 检查 `rate_limit_mbps`、并发数、存储侧限流 |
| `Discover/sec` 有值但 `Total files` 不增长 | 发现到的是 DB 里已有 key | 属于正常现象，不一定代表异常 |
| TUI 退出后任务也停止 | 按了 `q` | `q` 会取消当前同步，是预期行为 |
| 同步后仍有失败记录 | 自动重试已耗尽 | 用 `failed` 查看具体失败原因 |

## 8. 变更管理建议

- 配置文件中的凭证不要提交到 Git
- 生产环境配置建议单独保管
- 大规模变更前，先执行一次 `test`
- 首次迁移优先 `full`，稳定后改为 `incremental`
