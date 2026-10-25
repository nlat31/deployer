# 发布、部署与 Agent 自动更新

运行环境为 Linux amd64/arm64、Bash、util-linux/flock，具备 pidfd 支持。控制端、Agent 和 launcher **均以 root 运行**。不需要服务管理器、Go、Python、Node.js 或专用运行账号。Python 和 Go 仅用于构建及测试。

## 四个入口

控制端发行包直接包含 `setup.sh`、`start.sh`、`stop.sh`、`remove.sh` 和三个程序，无外层版本目录。可解压到 `/data/deployer` 后，在该目录运行这些脚本；`setup.sh` 安装但不会启动服务。Agent 包仍保留外层版本目录。

```bash
sudo mkdir -p /data/deployer
sudo tar -xzf deployer-server-0.2.0-linux-amd64.tar.gz -C /data/deployer
cd /data/deployer
sudo ./setup.sh
sudo ./start.sh
# 需要停止时：sudo ./stop.sh
```

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

## 上传与并行更新

控制台侧栏“Agent 更新”直接完成以下流程，无需手工调用 API：

1. 上传 `.manifest.json` 和 `.bin`；服务端校验签名、大小、摘要后列入版本选项。
2. 选择目标版本和 1～100 台同平台、支持更新的客户端，确认创建批次。离线节点等待上线。
3. 查看每台的排队、下载、试运行、提交及失败原因；可暂停或继续没有失败的批次。失败批次需修复后创建新批次。

管理员可以上传和发布，其他角色只可查看。私钥不上传，公钥配置仍遵循上一节。页面不会自动选择所有节点，也不会默认发布刚上传的版本。Agent 更新不包括 launcher；变更 launcher 仍需停服后 setup。

管理员 API 仍可用于自动化。以下请求使用登录返回的会话 cookie 和 CSRF token；若账号启用了 MFA，先完成正常 MFA 登录流程。

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

每批 1–100 台、同平台，最多 20 台并行；新批次需先暂停旧批次。离线未派发节点不占名额，其他在线节点可继续，已派发节点保留名额直到完成。Launcher 通过 60 秒稳定试运行并提交后，主控收到确认心跳即记录该设备完成、释放名额，不再额外观察 5 分钟。每台派发和完成状态均持久保存，重启主控不丢失并发名额；支持乱序完成，并兼容旧串行批次。失败暂停整个批次的后续派发，其他已交给 Launcher 的设备仍可能完成。未选中的主机保持原版本，因此可通过明确的节点列表固定版本。控制端从备份恢复后，如 Agent 发布文件缺失，批次自动暂停，普通心跳继续；重新上传同一签名发布后创建新批次。

暂停只阻止后续下发和新下载，已下载并交给本机 launcher 的更新仍可能完成。暂停不能取消正在进行的安全切换；需要停止本机时执行 stop.sh。

## 本机更新与恢复

Agent 通过现有 mTLS 下载，并验证 Ed25519 签名、平台、长度和 SHA-256；launcher 再次核验。主控对支持 gzip 的客户端使用持久缓存的压缩传输，现有 Go 客户端自动解压后仍校验原始签名程序；没有压缩支持或使用 Range 的请求保留原始表示。下载失败退避重试，旧程序继续运行。仅执行二进制，不执行更新包的安装脚本。

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
# 验证公网控制端包的根目录脚本与真实服务重复安装/启动/停止/移除
sudo python3 scripts/test-server-package.py dist/deployer-server-0.2.0-linux-amd64.tar.gz
# 真实签名更新和失败回退，包含完整 60 秒试运行
unshare -Ur python3 scripts/test-auto-update.py
# 真实 root 应用执行器测试（主机允许 user namespaces 时）
unshare -Ur env GOCACHE="$PWD/.cache/go-build" go test ./internal/supervisor ./internal/agent
```

生命周期测试验证重复 setup/start/stop/remove、业务 PID 保留、失败回退、应用停止失败时保留部署和拒绝符号链接。Go 测试覆盖签名篡改、下载损坏、试运行崩溃/卡死、中断恢复、停止优先和并行批次熔断。

## 控制台交互测试

```bash
npm --prefix web ci
cd web
npx playwright install chromium
npm test
```

浏览器测试使用隔离的接口样本，覆盖 multipart 上传、平台筛选、批量更新、暂停、失败提示、只读权限及删除确认取消。Go 集成测试通过真实 mTLS 服务与两台 Agent 验证删除任务、目录清理和回报；Direct supervisor 测试验证真实进程先停止后删除。


## 应用健康检查

HTTP 健康检查只使用 HTTP 状态码：2xx 表示正常，非 2xx、请求失败或主进程退出表示探测失败，响应正文中的错误消息会显示在后台部署记录中。Deployer 不解析 `ready`、初始化阶段、下载量或进度标记，也不识别“无进展”。应用负责在实际故障时从健康接口返回非 2xx 和具体错误原因；正常下载、校验和初始化应返回 2xx。

启动后，健康接口在配置的稳定期内持续成功即可提交部署；此时应用可以仍在初始化。`timeout_seconds` 仅用于启动健康检查的等待上限，稳定期保留。它不限制应用内部下载或初始化总时长。应用后续发生故障，由自身健康接口报告，Agent 仅上报健康状态和错误消息，不自动回退、停止或重启 App。管理员在后台手动切换版本或启停。

Loom Worker 的 `/healthz` 和 `/readyz` 正常初始化均返回 HTTP 200、`healthy:true`、`ready:false`、空 `error`；`ready` 仅供查看业务准备情况。真实组件错误或主控连接错误返回 HTTP 503 和具体原因。无需新的 Deployer Agent 健康响应协议，已经上传的旧 Worker 包仍需更新才具备该行为。


## App 失败处理

App 不提供自动回退选项。准备阶段失败时旧版本继续运行；旧版本停止、新版本已尝试启动后发生失败，记录选中的目标版本、实际进程状态及错误，保留版本目录，等待管理员手动切换。健康检查失败不会停止仍在运行的目标进程，也不会重启旧版本。无法确认旧进程已停止时不启动目标版本，标记需要管理员核对；中断操作的恢复同样只核对和上报，不隐式切回旧版本。

运行期间健康接口的非 2xx 响应正文随 Agent 心跳传至后台设备列表；应用恢复正常后清除当前健康错误，历史任务失败原因保留。App 进程退出同样只上报，由管理员手动启动或切换。主机或 Agent 重启后只核对并上报 App 状态，不按旧的 running 意图自动启动 App，需管理员明确下发启动命令；Agent 自身的签名自动更新、试运行和失败回退不受 App 策略影响。


启动命令总会执行 App 的 start.sh，重复调用是否幂等由 App 自己负责。已有受管进程时，Deployer 注入 APP_MAIN_PID 并保留原进程记录，等待脚本返回；不会替 App 先停止再启动。Loom Worker 已在自己的 start.sh 实现重复启动处理。Agent 心跳允许同一协议中的未知可选字段，确保旧客户端带已删除的 auto_rollback 状态仍能连接并领取更新；管理员操作接口仍严格拒绝该已删除选项。

## 待发布：版本部署与启动时间

心跳版本清单新增 `started_at`，从持久化 App 进程记录读取最近实际启动时间。已有 `installed_at` 调整为包校验落盘后填写，避免失败下载提前报告部署时间。设备列表和版本管理展示两个时间及实际运行版本。两个字段都是客户端事件时间，不使用控制端收到心跳的时间替代；旧记录缺少启动时间时保留未知。

需要更新控制端页面及 Agent 后才具备完整展示和采集能力。本项已随 0.2.4 公网发行包交付。测试覆盖完整版本清单的 mTLS 心跳存储、下载失败、重复安装/启动、停止后保留、重新启动刷新、版本切换隔离及 Agent 重启接管。

### 0.2.4 设备列表与心跳目录同步

移除“应用部署”页面和 App 回退动作/引用。设备列表必须选择 App 或未部署，提供版本切换、启停、日志、版本及时间查看。仓库删除立即生效；Agent 心跳接收 deleted 并先停再删，不建立删除任务或标记。停止失败的目录保留，cleanup_error 在设备列表展示，后续心跳重试。发布需同时更新控制端和 Agent；历史未采集的启动时间仍显示未记录。
