# Windows 安装、配对与无扬声器方案

建议使用 Windows 10 1703 或更新版本，优先 Windows 11 x64。发送进程运行期间请求 1 ms 定时精度用于音频节拍，退出后释放，不持久修改系统设置。

目标是把 DESKTOP-CUNKY 的系统声音送到 Mac `192.168.2.20`。本次 Windows 离线，尚未做真实 WASAPI、驱动、计划任务或开关机验收；`192.168.2.21` 只在 Mac ARP 缓存中出现，身份未确认。

## 先确认播放端点

WASAPI endpoint loopback 捕获的是一个活动的播放端点的混音，并不直接捕获“整个操作系统”中的所有声音。应用必须把声音输出到被采集的端点。独占模式和受保护内容存在限制。

Windows 没接音箱并不一定代表没有端点：显示器 HDMI、声卡或虚拟播放设备可能仍为 ACTIVE。先在 Windows“设置 → 系统 → 声音”检查，再运行 `remote-au.exe devices --json`。以实际运行时的编号为准，编号可能随设备变化。可用 `device --capture INDEX` 保存 loopback 端点，或用 `send --source loopback --device INDEX` 临时选择。

如果所有播放端点均禁用、断开或不存在，普通 endpoint loopback 无法开流。Rust Daemon 会记录错误并重试，但不能凭空创建 Windows 播放端点。

### 推荐无端点时使用 VB-CABLE

1. 由本人从 [VB-Audio 官网](https://vb-audio.com/Cable/) 下载，核对签名，以管理员身份安装。它是第三方音频驱动，安装后会持久存在；官网要求安装和卸载后重启。脚本不会自动安装它。
2. Windows 默认输出改为 **CABLE Input (VB-Audio Virtual Cable)**。已有应用可能需要单独更改输出或重启。
3. 运行 `devices`，选择 **CABLE Input** 对应的播放端点作 loopback 源。名称中的 Input 是虚拟线缆的播放侧；CABLE Output 是录音侧，本程序的 endpoint loopback 不选它。
4. Windows 不会从真实扬声器发声，Mac 接收端播放。不要为了“本地不出声”再把该虚拟端点系统静音或音量设为 0；这样可能也使采集内容静音，行为因驱动而异。
5. 要回滚，恢复原默认输出，从 Windows 应用/设备管理卸载 VB-CABLE，按厂商要求重启。

声卡若提供可保持 ACTIVE 的输出，也可直接使用。VoiceMeeter 可以做到但更复杂，不是此场景的首选。硬件 HDMI dummy plug 等也属于用户另行决定的设备方案。

### 静音、音量和默认设备变化

不要假设 Windows 的 mute 对 loopback 一定无影响。请分别验证端点未静音、系统静音、音量降为零三个情况。控制 Mac 播放音量优先使用接收端 `volume --master`，以免影响源信号。

切换默认播放设备后，采集端需要重建流；若钉死了设备编号，改回 `device --reset` 并重启任务，或选择新的端点。RDP 可能切换默认端点、引入远程音频重定向，第一次验收应在本地桌面进行。

## 安装与配对

解压交付包 `remote-au-windows-x64`，保持 `remote-au.exe` 和 `scripts/` 结构，建议先检查 SHA256。以下均在普通 PowerShell 当前用户下操作，管理员只用于可选防火墙步骤。

```powershell
Set-Location C:\path\to\remote-au-windows-x64
Get-FileHash .\remote-au.exe -Algorithm SHA256
.\scripts\install-daemon.ps1 -BinarySource .\remote-au.exe
```

如果执行策略阻止脚本，单次执行可用：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\install-daemon.ps1 -BinarySource .\remote-au.exe
```

安装位置为 `%LOCALAPPDATA%\remote-au`，计划任务 `remote-au-daemon` 在当前用户登录后隐藏运行，失败后重启，持续运行不受默认三天时长限制。不在登录前运行，也不保存用户密码。首次未配对时不会启动采集。

Cunky 在 Mac 上创建邀请文件，使用可信私密渠道传给 Windows。邀请里有密钥，不要放在公开共享目录或发进报告。

```powershell
$exe = "$env:LOCALAPPDATA\remote-au\bin\remote-au.exe"
& $exe pair --import-file C:\private\invitation.json
# 成功后手动删除 invitation.json 的传输副本。
& $exe devices --json
Start-ScheduledTask -TaskName remote-au-daemon
& $exe status
```

Mac 日常接收 Daemon 的配置目录与 CLI 默认目录不同时，Mac 命令必须带对应的 `--config-dir`。导出的同一份群组邀请可用于别的节点。

默认自动发现，同一邀请认证通过后优先直连链路。发现失败或只想固定 Mac：

```powershell
& $exe select-server 192.168.2.20:47000
Stop-ScheduledTask -TaskName remote-au-daemon
Start-ScheduledTask -TaskName remote-au-daemon
# 恢复自动发现：
& $exe select-server auto
```

检查 `ipconfig` 和 `hostname`，确认电脑名为 DESKTOP-CUNKY，确认直连接口实际地址属于 `192.168.2.0/24`，不要仅依据 Mac 的旧 ARP 表把 `.21` 当成已确认身份。

## 防火墙

通常发送端 UDP 出站和对应回应由状态防火墙允许。若本机策略阻挡发现，管理员 PowerShell 显式运行：

```powershell
.\scripts\add-firewall-rules.ps1 -ProgramPath "$env:LOCALAPPDATA\remote-au\bin\remote-au.exe"
```

规则限定本程序、Private 配置文件，入站限制 LocalSubnet；音频 UDP 47000，发现 UDP 47001/48001/49001。公网/域网络不放开。若直连接口被识别为 Public，需要用户先确认可信再自行设为 Private，脚本不更改网络类别。企业策略可能覆盖本地规则。

## 停止、卸载、切换角色

```powershell
Stop-ScheduledTask -TaskName remote-au-daemon
.\scripts\uninstall-daemon.ps1
# 明确删除本应用配置、密钥、日志、二进制：
.\scripts\uninstall-daemon.ps1 -Purge
# 若添加过防火墙规则，用管理员执行：
.\scripts\uninstall-firewall-rules.ps1
```

要让 Windows 当常驻服务器，先停止 Mac 的接收启动项，再重新运行安装器选择角色：

```powershell
.\scripts\install-daemon.ps1 -Role server
```

恢复采集端用 `-Role send`，仍使用同一个任务和配对目录，不需要重新配对。其他节点保存 Windows 的服务器地址或使用自动发现。Mac 安装器默认 `server`，也可用 `REMOTE_AU_ROLE=send REMOTE_AU_SOURCE=mic` 安装为采集端；系统声音需已配置的虚拟输入设备。

## Windows 开机后验收

- 本地播放音乐，Mac `status` 显示有效流且音箱有声；Windows 本地无须接音箱。
- 关机后 Mac 留在等待状态；开机并登录后无需手工启动或重新配对。
- Mac 接收端重启、网线短暂断开再接回后可恢复。
- 记录真实端点名称、驱动、系统静音对环回的影响。
- 查看 `%LOCALAPPDATA%\remote-au\logs\daemon.log`；日志启动时超过 5 MB 会轮转，长期运行的日志需定期检查。

## 依据

- [微软 Loopback Recording](https://learn.microsoft.com/en-us/windows/win32/coreaudio/loopback-recording)：需要播放端点和共享模式。Session 0 环回本身可跨会话采集；选择登录任务是为了用户端点、权限与配置路径管理，不是因为 API 禁止服务采集。
- [微软端点状态](https://learn.microsoft.com/en-us/windows/win32/coreaudio/device-state-xxx-constants)：ACTIVE、DISABLED、NOTPRESENT、UNPLUGGED 的区别。
- [微软按进程环回示例](https://github.com/microsoft/Windows-classic-samples/tree/main/Samples/ApplicationLoopback)：Windows build 20348 及之后的另一条技术路线，本版未实现，不承诺解决所有无端点情形。
- [VB-CABLE 官方安装说明](https://vb-audio.com/Cable/)：管理员安装，安装/卸载后重启。
