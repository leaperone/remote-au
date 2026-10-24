# remote-au

将 Windows 的系统播放声音通过局域网传到接音箱的 Mac，支持多台采集端混音。Rust 版以常驻进程运行，发送端离线时接收端持续等待，恢复后自动连接。

Rust 代码位于 `src/`，Go 代码保留在 `cmd/`、`internal/`，旧版使用说明见 [legacy-go.md](docs/legacy-go.md)。两版协议不同，不互通；现有 npm/Go 发布流程仍发布旧版，本 PR 不触发版本发布。

## 构建

需要 Rust stable。无需额外音频编码库；Windows 使用 WASAPI，Mac 使用 CoreAudio。

```sh
cargo test --locked
cargo build --release --locked
```

CI 原生构建 macOS Apple Silicon 和 Windows x64，提供二进制及安装脚本 artifact。Linux 暂未列入 Rust 验收平台。

在系统盘空间较小时，将以下变量指向有空间的卷，并通过 rustup 的 `--no-modify-path` 安装：

```sh
export RUSTUP_HOME=/Volumes/CodingSpace/toolchains/remote-au/rustup
export CARGO_HOME=/Volumes/CodingSpace/toolchains/remote-au/cargo
export CARGO_TARGET_DIR=/Volumes/CodingSpace/toolchains/remote-au/target
export PATH="$CARGO_HOME/bin:$PATH"
```

## 配对并运行

接收端先创建邀请。邀请文件包含密钥，应只经可信渠道传递，不要提交到 Git 或粘贴到日志中。

```sh
remote-au --config-dir /path/to/private/config pair
remote-au --config-dir /path/to/private/config pair --export-file /private/transfer/invitation.json
remote-au --config-dir /path/to/private/config server
```

发送端导入同一份邀请，然后运行：

```powershell
remote-au.exe pair --import-file .\invitation.json
remote-au.exe send --source loopback
```

导入后删除邀请的传输副本。未配对时拒绝推流。共享邀请内的所有节点相互信任；撤销其中一台的权限，需要在服务器 `pair --rotate`，重新给保留的节点配对，并重启各进程。

自动发现优先使用 `192.168.2.0/24`。可保存明确的接收端地址，或恢复自动发现；重启发送进程生效。

```sh
remote-au select-server 192.168.2.20:47000
remote-au select-server auto
remote-au nodes
remote-au status
remote-au volume --master 0.7
remote-au devices --json
remote-au device --output 1
remote-au device --capture 0
remote-au device --reset
```

音量、设备和选服设置按 `--config-dir` 隔离。Windows loopback 的 `--device` 使用播放端点编号。Mac 采集系统声音需已安装的虚拟设备并作为输入选择，默认不安装驱动。

接收角色由 `server` 选择，发送角色由 `send` 选择，与平台无关。切换角色前停掉旧进程或启动项，同一配置目录同时运行一个角色。Windows 无扬声器时的端点方案与逐步安装说明见 [Windows 指南](docs/windows.md)。

## 常驻启动与回滚

Mac 用户级 LaunchAgent：将二进制放在 `/Volumes/CodingSpace/remote-au-runtime/bin/remote-au`，先在 `~/Library/Application Support/remote-au/config` 完成配对，再运行：

```sh
bash scripts/macos/install-launchagent.sh
bash scripts/macos/uninstall-launchagent.sh
```

正常卸载保留配对和数据，显式 `PURGE=1` 才删除默认运行目录内本应用文件。LaunchAgent 在用户登录后启动，退出后自动拉起；需要外置卷已挂载。运行配置与少量日志使用用户 Library，工具链和二进制继续外置。macOS 若要求访问可移动宗卷，需本人允许；普通终端验证通过并不代表 LaunchAgent 已获得同样权限。

Windows 安装器默认 `-Role send`，可用 `-Role server` 切换常驻服务器。Mac 安装器默认 `REMOTE_AU_ROLE=server`，可用 `REMOTE_AU_ROLE=send REMOTE_AU_SOURCE=mic` 改为采集端。切换服务器前先停止原服务器启动项。

Windows 登录任务以当前交互用户运行，适合访问桌面音频会话，无需保存密码：

```powershell
.\scripts\windows\install-daemon.ps1 -BinarySource .\remote-au.exe
# 导入邀请后：
Start-ScheduledTask -TaskName remote-au-daemon
# 卸载，保留配对：
.\scripts\windows\uninstall-daemon.ps1
```

防火墙脚本是单独的管理员步骤，仅对本程序和 Private 网络添加 UDP 规则，不安装驱动、不改变默认音频设备。Windows 登录前不会运行音频采集。

## 链路与边界

最多 8 路有效流，离线约 10 秒后回收混音槽。固定 48 kHz 双声道、5 ms 音频包，UDP 数据报不超过 1200 字节。直连链路优先无损 PCM，较弱链路自动使用 IMA ADPCM 双声道或 24 kHz 单声道。ADPCM 有损，弱链路音质会降低。本版不包含 Opus。

缓冲目标 20–150 ms，根据 RTT、到达抖动、丢包与欠载变化。分档探测最多测试 8 Mbps，单次流量不超过 128 KiB；报告的速率同时受实际发送和接收间隔约束，表示已测试范围，不是网卡物理带宽。链路突变时需要收敛，恢复时逐级升档。会话历史最多 4096 项，满后自动更换 epoch 并让节点重新认证。

## 验证

合成音源、两个独立进程、故障注入及重启恢复检查：

```sh
python3 tools/verify_daemon.py --binary /path/to/remote-au --output /path/to/evidence
# 添加 --playback 后通过默认扬声器低音量播放测试音。
```

软件 RTT 和缓冲统计不等同于声学端到端延迟。Windows CI 构建也不代表已验证真实硬件上的 WASAPI 或驱动行为。

## License

[AGPL-3.0-or-later](LICENSE) © leaperone.
