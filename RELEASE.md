# 发布、部署与 Agent 自动更新

运行环境为 Linux amd64/arm64、Bash、util-linux/flock，具备 pidfd 支持。控制端、Agent 和 launcher **均以 root 运行**。不需要服务管理器、Go、Python、Node.js 或专用运行账号。Python 和 Go 仅用于构建及测试。

## 四个入口

```bash
./release-server.sh 1.0.0
./release-agent.sh 1.0.0
# 在目标机器解压对应角色的 tar.gz，然后在包目录执行：
sudo ./setup.sh
sudoedit /data/deployer/config/agent.env  # server 包则为 server.env
sudo ./start.sh
sudo ./stop.sh
sudo ./remove.sh
```

源码目录也提供 `./setup.sh [agent|server]`、`./start.sh [agent|server]`、`./stop.sh [agent|server]`；默认 agent，setup 使用 `dist/` 的构建结果。源码 `./remove.sh [agent|server|all]` 默认移除两个角色。

- setup：幂等安装。相同内容不切换，已有配置、证书和数据保留；首次不启动。运行中安装新程序会等待安全窗口、试运行并在失败时回退。停止状态安装的新程序在下次 start 时验证。launcher 本身变更须先停止两个角色，避免正在运行的守护器与磁盘版本不一致。
- start：后台启动独立会话中的 launcher，等待服务本机就绪；正常运行时重复执行不重启。正在恢复或排空时返回非零，请检查状态后重试。
- stop：持久化停止意图，Agent 暂停接单并等待当前任务/自愈完成，再停止服务和 launcher。保留业务进程。安全停止超时返回非零，不强杀业务或正在执行任务的 Agent；停止意图保留。
- remove：先 stop；Agent 使用当前程序和配置的数据目录执行 `--stop-apps`，确认应用停止后移除该角色程序和生命周期入口。停止失败保留部署。**配置、证书、数据库、业务数据、日志均保留**，重复移除成功。共享 launcher 留作幂等操作入口。清空数据不属于卸载操作。

安装后的操作入口为 `/data/deployer/lifecycle/{agent,server}/{start,stop,remove}.sh`。重新部署使用发行包的 setup.sh。测试或隔离部署可设置绝对路径 `DEPLOYER_ROOT`；它仅改变安装位置，配置文件中的数据及证书路径需自行设置。

配置为逐行的字面 `KEY=value`，不执行 shell 命令，不展开 `$VAR`，不去除引号。私钥和配置目录权限为 root 独占。服务日志位于 `logs/agent.log`、`logs/server.log`，每个角色保留当前和上一份，每份约 16 MiB。

```bash
sudo /data/deployer/bin/deployer-launcher status --root /data/deployer --role agent
sudo tail -f /data/deployer/logs/agent.log
```

数据仍在 `agent/` 和 `controller/`；程序位于 `releases/<role>/<sha256>/`，`bin/deployer-<role>` 是原子切换的链接。`updater/<role>.json` 独立保存切换事务和失败摘要，不能在升级中手动删除。第一版保留历史程序，不自动清理业务目录或发布程序。

launcher 负责服务异常退出后的退避重启。launcher 自身退出或主机重启后需要重新执行 start.sh；没有默认的开机启动依赖。检测到仍存活的孤儿服务时拒绝重复启动，请先 stop.sh 再 start.sh。

## TLS 与首次接入

```bash
bash scripts/pki.sh init ./pki deployer.example.com
bash scripts/pki.sh node ./pki node-a
```

控制端配置 `server-cert.pem`、`server-key.pem`、`ca.pem`；Agent 配置信任的 `ca.pem`，客户端证书和私钥缺失时首次启动自动生成和申请。CA 私钥仅保留在签发机器。服务端证书 SAN 必须匹配 `DEPLOYER_SERVER` 地址。

通用公网包仍通过 `./release-public.sh VERSION` 生成；同一个 Agent 包可用于多台机器，不包含共享客户端私钥。`DEPLOYER_RELEASE_PKI` 指定构建时嵌入的证书来源，控制端私钥安装为 root:root 0600。

## 签名发布

首次创建发布密钥，在离线/受控构建机保存私钥：

```bash
./dist/deployerctl update-keygen --key ./update-private.key --public ./update-public.key
DEPLOYER_UPDATE_PUBLIC_KEY="$PWD/update-public.key" \
DEPLOYER_UPDATE_SIGNING_KEY="$PWD/update-private.key" \
./release-agent.sh 1.1.0
```

输出安装包以及 `dist/deployer-agent-1.1.0-linux-amd64.bin` 和同名前缀的 `.manifest.json`。设置 `GOARCH=arm64` 构建另一平台。签名覆盖版本、平台、大小、摘要和兼容格式；私钥不进入发行包。manifest 格式为 1，当前兼容标记为 `protocol3-state3-process1`。不兼容协议、数据库或进程身份格式的升级禁止复用该标记，不走此自动更新流程。

首次安装时将公钥放入控制端和 Agent 的 `/data/deployer/config/update-public.key`，也可通过构建参数嵌入新安装包。已有公钥不会被 setup 覆盖。服务端可用 `DEPLOYER_UPDATE_PUBLIC_KEY` 指定公钥路径；Agent 使用安装目录的固定路径。缺少公钥时仍能运行，但不上报自动更新能力。

## 上传与串行更新

第一版提供管理员 API，下发目标版本无需更改 Agent 配置。主机页面显示版本、试运行/回退状态和错误。以下请求使用登录返回的会话 cookie 和 CSRF token；若账号启用了 MFA，先完成正常 MFA 登录流程。

```bash
# 使用真实控制端地址、受信任 CA 和登录会话；不要关闭 TLS 校验。
curl --cacert ca.pem -b cookies.txt -H "X-CSRF-Token: $CSRF" \
  -F 'manifest=<dist/deployer-agent-1.1.0-linux-amd64.manifest.json' \
  -F 'binary=@dist/deployer-agent-1.1.0-linux-amd64.bin' \
  https://deployer.example.com:7710/api/agent-releases

curl --cacert ca.pem -b cookies.txt -H "X-CSRF-Token: $CSRF" \
  -H 'Content-Type: application/json' \
  -d '{"digest":"发布返回的 SHA-256","nodes":["node-a","node-b"]}' \
  https://deployer.example.com:7710/api/agent-rollout

# 查看批次，或暂停后续下发
curl --cacert ca.pem -b cookies.txt https://deployer.example.com:7710/api/agent-rollout
curl --cacert ca.pem -b cookies.txt -H "X-CSRF-Token: $CSRF" \
  -H 'Content-Type: application/json' -d '{"paused":true}' \
  https://deployer.example.com:7710/api/agent-rollout
```

每批 1–100 台、同平台，一次只放行一台；新批次需先暂停旧批次。离线节点阻塞其队列位置，重连后继续。某台提交成功且连续心跳观察 5 分钟后，才放行下一台；心跳中断超过 30 秒重新计时。失败暂停整个批次。未选中的主机保持原版本，因此可通过明确的节点列表固定版本。控制端从备份恢复后，如 Agent 发布文件缺失，批次自动暂停，普通心跳继续；重新上传同一签名发布后创建新批次。

暂停只阻止后续下发和新下载，已下载并交给本机 launcher 的更新仍可能完成。暂停不能取消正在进行的安全切换；需要停止本机时执行 stop.sh。

## 本机更新与恢复

Agent 通过现有 mTLS 下载，并验证 Ed25519 签名、平台、长度和 SHA-256；launcher 再次核验。下载失败退避重试，旧程序继续运行。仅执行二进制，不执行更新包的安装脚本。

1. 暂停接单，本地门控等待任务、自愈和维护退出，业务进程继续运行。
2. 10 分钟未进入安全窗口则延后/标记失败，保留旧程序，不强杀。
3. 停止旧 Agent，写入独立事务，原子切换链接并启动候选程序。
4. 候选程序不接任务、不启动后台自愈；确认本地恢复、数据库及业务进程身份可读，主循环持续报告就绪 60 秒后提交。180 秒未完成验证或中途退出则回退。
5. 回退只恢复程序，不恢复旧数据库。停止意图优先于升级和崩溃重启。同一失败摘要被本机阻止重复自动尝试。

下次 start 遇到中断的试运行会先恢复旧程序。若有仍存活的孤儿服务，先安全停止后再恢复。修复失败版本应发布新内容/新摘要；不要通过删失败记录来强行重试。手动 setup 是管理员明确重试入口。下载中的磁盘满、校验失败不会切换程序。

## 从旧安装迁移

旧版本曾由 systemd 管理，必须先用旧环境的管理方式停止并禁用旧服务，确认不会再次拉起，再执行新版 setup/start。新版脚本不调用、安装或依赖 systemd，也不会自动操作旧服务管理器。原有配置和业务数据可直接保留；首次切换前备份数据库和密钥。不自动迁移不兼容状态结构。

## 测试

```bash
npm --prefix web run build
GOCACHE="$PWD/.cache/go-build" go test -race ./...
GOCACHE="$PWD/.cache/go-build" go vet ./...
# Linux 用户命名空间中的 root；只访问临时安装目录
unshare -Ur python3 scripts/test-lifecycle.py
# 真实签名更新和失败回退，包含完整 60 秒试运行
unshare -Ur python3 scripts/test-auto-update.py
# 真实 root 应用执行器测试（主机允许 user namespaces 时）
unshare -Ur env GOCACHE="$PWD/.cache/go-build" go test ./internal/supervisor ./internal/agent
```

生命周期测试验证重复 setup/start/stop/remove、业务 PID 保留、失败回退、应用停止失败时保留部署和拒绝符号链接。Go 测试覆盖签名篡改、下载损坏、试运行崩溃/卡死、中断恢复、停止优先和串行批次熔断。
