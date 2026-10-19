# Deployer

独立的应用部署工具。一个控制端管理多台 Linux 主机，Agent 统一执行 App 的四个生命周期脚本。应用仓库、控制页面、数据库、构建和测试都在本目录内，可整体移动后使用。

## 已实现

- 内嵌中文控制页面，管理员、操作员、只读用户，登录会话与 CSRF 校验。
- `App / 版本` 完整包仓库，SHA-256 校验、不可覆盖版本、路径和展开大小校验。
- Agent 与控制端采用 mTLS 双向证书认证，合法心跳自动登记；主机列表显示在线/离线，离线主机由管理员手动删除。
- 安装、部署并启动、启停、重启、版本切换、失败回退、取消回退保留、解除部署。
- 清理指定版本的 data、删除本地版本、删除仓库版本并同步清理所有客户端目录。
- 多主机逐台部署，首个失败后停止派发后续主机；批次和派发进度持久化。
- 操作阶段和日志、应用进程文件日志、断线结果重传、重复任务去重、Agent 重启恢复和主机重启恢复。
- 不确定状态阻止后续变更，可通过“停止并解除阻塞”核对并停止该 App 的所有本地版本。

支持 Linux amd64/arm64、Bash、flock 和 pidfd。控制端与 Agent 均以 root 运行，由独立 launcher 管理，无服务管理器依赖。同一主机上的同一 App 同时运行一个版本；切换有短暂停机。应用数据按版本隔离，回退程序不会回滚外部数据库。业务进程异常退出或持续不健康时自动恢复，连续失败按 10 至 320 秒退避重试；主动停止后保持停止。

## 构建

独立控制端/客户端发布包使用 `./release-server.sh VERSION` 和 `./release-agent.sh VERSION`，安装、证书与校验步骤见 [RELEASE.md](RELEASE.md)。

需要 Go 1.24 或更新版本、Node.js 20、npm。

```bash
./build.sh
./scripts/package-sample.sh 1.0.0
./scripts/package-sample.sh 1.1.0
```

输出在 `dist/`：`deployer-server`、`deployer-agent`、`deployerctl` 及样例包。前端通过 `go:embed` 编入服务端，运行环境不需要 Node.js。前端依赖由本项目的 `web/package-lock.json` 固定。

只构建 Go 程序前先执行 `npm --prefix web ci` 和 `npm --prefix web run build`。本仓库保留当前页面构建产物，方便直接执行 `go build -buildvcs=false ./...`；修改页面后需重新构建。

## 启动控制端

首次创建数据库时内置帐号为 `admin`，密码固定为 `xScan!@&`。登录页只保留用户名、密码和登录按钮。首次登录后可在“我的帐号”修改密码、绑定 Google Authenticator（或兼容 TOTP 的验证器）。密码修改后持久保存，重启不会恢复默认密码。

### 证书准备

控制端与 Agent 均必须配置证书；Agent 不接受 HTTP、不跳过服务端证书校验，没有节点 Token 或接入授权页面；首次启动通过 HTTPS `/enroll` 自动申请客户端证书，此接口不能执行 Agent 业务操作。控制端信任的客户端 CA 校验所有业务连接。通用公网包使用 `./release-public.sh VERSION` 构建，同一个 Agent 包可安装到多台服务器，各机首次启动独立生成私钥、申请证书并通过合法 mTLS 心跳登记，无需预签发。证书可由现有内部 CA 签发，或用随附脚本创建专用 CA（签发机器需 OpenSSL 1.1.1+）：

```bash
bash scripts/pki.sh init ./pki deployer.example.com
bash scripts/pki.sh node ./pki node-a
bash scripts/pki.sh node ./pki node-b
```

`init` 创建 CA 和服务端证书；`node` 为每台主机签发证书。输出目录必须不存在。默认有效期为 3650 天，可在签发时用 `DEPLOYER_CERT_DAYS` 指定。CA 私钥 `ca-key.pem` 仅保留在签发机器，不复制到控制端或 Agent。

服务端证书的 DNS/IP SAN 须与 Agent 使用的控制端地址匹配。客户端证书须包含 `clientAuth` 用途，以及唯一 URI SAN：`spiffe://deployer/nodes/node-a`。末段是固定主机 ID，支持 1 至 96 位字母、数字、点、下划线及短横线，首位须为字母或数字。同一主机续期时保持 ID 不变，每台主机使用独立私钥。

```bash
./dist/deployer-server \
  --listen 0.0.0.0:7710 --data ./data/server \
  --tls-cert ./pki/server/cert.pem --tls-key ./pki/server/key.pem \
  --client-ca ./pki/ca.pem
```

浏览器打开 `https://deployer.example.com:7710`，信任服务端证书后正常使用帐号登录，浏览器无需客户端证书。Agent 接口强制要求 mTLS。若仅在本机开发，可额外设置 `--ui-listen 127.0.0.1:7712` 提供本机 HTTP 页面；该监听不能访问 Agent 接口，Agent 仍连接 HTTPS 主监听。

“我的帐号”提供改密、验证码绑定和恢复码。证书续期替换证书及私钥文件后，新 TLS 握手自动加载完整有效证书对，主机身份不变；替换尚未完成时保留已加载的有效证书。CA 文件更新也会生效，已建立连接上的客户端请求会再次按当前 CA 和有效期校验。

## 安装生产服务和接入主机

在控制端安装服务，然后编辑环境文件，配置下述证书后再启动：

```bash
sudo bash scripts/install.sh server
sudoedit /data/deployer/config/server.env
# setup 不启动；配置完成后执行 start.sh server
```

在目标主机放置本项目的发行文件并安装 Agent：

```bash
sudo bash scripts/install.sh agent
sudoedit /data/deployer/config/agent.env
# setup 不启动；配置完成后执行 start.sh agent
```

控制端需要 `/data/deployer/config/server-cert.pem`、`server-key.pem` 和 `ca.pem`；`server.env` 配置对应路径。控制端服务账号需要能够读取服务端私钥：

```bash
sudo install -m 0644 ./pki/server/cert.pem /data/deployer/config/server-cert.pem
sudo install -m 0644 ./pki/ca.pem /data/deployer/config/ca.pem
sudo install -m 0600 -o root -g root \
  ./pki/server/key.pem /data/deployer/config/server-key.pem
sudo /data/deployer/lifecycle/server/start.sh
```

目标主机需要信任服务端的 `ca.pem`；`client-cert.pem`、`client-key.pem` 缺失时首次启动自动生成和申请。配置 `agent.env` 的 `DEPLOYER_SERVER`、`DEPLOYER_CA`、`DEPLOYER_CLIENT_CERT`、`DEPLOYER_CLIENT_KEY`。Agent 私钥仅供 root 读取。证书和配置准备好后执行 `sudo /data/deployer/lifecycle/agent/start.sh`，首次合法心跳自动保存主机信息，无需先在页面添加。

“主机列表”在 30 秒没有有效心跳后显示离线，离线记录不会自动删除。管理员可以删除离线主机；在线主机须先停止 Agent。删除只移除控制端主机、安装清单和部署记录，终止相关待执行任务及批次并保留审计，不能停止失联主机上的进程或删除业务数据。合法心跳再次到达时重新登记，已有本地部署进入待核对状态，执行“停止并解除阻塞”后继续管理。

安装器幂等安装程序并保留配置和数据。控制端、Agent 与 launcher 均以 root 运行。使用发行包的 `setup.sh / start.sh / stop.sh / remove.sh` 管理，或安装目录 `lifecycle/{agent,server}/` 下的启停入口。stop 保留业务进程，remove 先停止业务再移除部署，保留配置、证书、数据库和业务数据。launcher 崩溃或主机重启后需重新执行 start。

控制台“Agent 更新”支持上传签名清单与程序、选择同平台客户端批量更新、查看各节点进度和失败原因、暂停或继续批次。管理员可操作，只读用户可查看。逐台更新包含 60 秒试运行、5 分钟稳定观察和失败回退；主机列表也显示版本及更新结果。发布公钥、管理员 API、兼容性约束和旧安装迁移见 [RELEASE.md](RELEASE.md)。

本地测试 Agent 也需要以 root 启动：

```bash
sudo ./dist/deployer-agent --server https://deployer.example.com:7710 \
  --data ./data/agent-dev --name dev-node \
  --ca ./pki/ca.pem --cert ./pki/nodes/node-a/cert.pem \
  --key ./pki/nodes/node-a/key.pem
```

应用进程 stdout/stderr 写入版本的 `logs/process.log`；观察时超过 16 MiB 则保留末尾 64 KiB。应用 `start.sh` 保持前台运行，推荐使用 `exec` 启动主程序；`stop.sh` 可通过 `APP_MAIN_PID` 优雅停止。Agent 持久化进程 PID、启动时间和独立身份，负责异常恢复和残留进程清理；应用脚本直接以 root 运行。

## 接入自己的 App

上传 `tar.gz`，解压根目录必须直接有 `setup.sh`、`start.sh`、`stop.sh`、`clean.sh`，可以包含程序、依赖及 `config.defaults/`。不能有外层包装目录、符号链接、硬链接或设备文件。支持可选 `app.yaml`：

```yaml
app_id: my-app
version: "1.0.0"
os: linux
arch: amd64
```

这些元数据必须与上传页面填写值一致。一个 App 的一个版本对应一个平台。上传大小上限 512 MiB，展开上限 2 GiB，文件项上限 20,000。

| 脚本 | 要求 |
| --- | --- |
| `setup.sh` | 120 秒内完成，只初始化当前版本，可重复执行，不启动后台进程 |
| `start.sh` | 用 `exec` 运行前台主进程，不使用 nohup 或自行注册系统服务 |
| `stop.sh` | 30 秒内优雅停止，支持重复执行；可使用 Agent 注入的 `APP_MAIN_PID` |
| `clean.sh` | 60 秒内清空当前版本 data 内容；仅此目录可写，保留 data 目录本身 |

工具注入 `APP_ID`、`APP_VERSION`、`APP_RELEASE_DIR`、`APP_DATA_DIR`、`APP_CONFIG_DIR`、`APP_LOG_DIR`、`APP_RUN_DIR` 和 `DEPLOY_OPERATION_ID`。脚本工作目录是版本根目录。`start.sh` 的退出码不是健康检查结果，Agent 会验证进程并执行配置的本机 HTTP/TCP 探测。

自定义环境配置在部署窗口使用 `KEY=value`，有值时替换整个配置，留空保留当前配置，勾选“清空环境配置”可显式清除。程序按自己的逻辑读取这些环境变量；工具将 `config.defaults/` 中尚不存在的文件复制到本版本 `config/`，不自动改写应用专有配置格式。

脚本在只读系统视图下运行，setup/start/stop 只允许写该版本的目录，clean 只允许写 data。程序需要临时文件时使用 `APP_RUN_DIR`。包必须来自可信发布者；本工具不提供对不可信代码的多租户安全隔离。

可先运行包检查：

```bash
./dist/deployerctl check-package --app my-app --version 1.0.0 \
  --arch amd64 my-app-1.0.0.tar.gz
```

## 验证样例

1. 创建标识为 `sample-app` 的应用，健康检查选择 HTTP，地址 `http://127.0.0.1:8877/health`。
2. 上传构建出的 `1.0.0` 和 `1.1.0` 样例包。
3. 选择主机部署 `1.0.0`，查看任务及进程日志，再切换到 `1.1.0`。
4. 在切换时设置 `FAIL_START=1`，观察启动失败和恢复旧版本。回退使用此前的配置，因此旧版本不会继承失败参数。
5. 停止当前版本后清理 data，再启动，验证会重新初始化。

样例服务只绑定目标主机 `127.0.0.1:8877`。环境变量 `PORT` 可以修改端口，`MESSAGE` 可以修改返回内容，`FAIL_START=1` 用于故障演练。

## 数据和恢复

控制端使用 `state/controller.db`，Agent 使用 `state/agent.db`；元数据按实体保存在 SQLite records 表，操作事件单独保存。记录采用 AES-GCM 加密，密钥是同目录的 `master.key`。备份与恢复必须同时保留密钥、数据库和版本目录；丢失密钥不能恢复数据。不要复制正在写入的 SQLite 单个主文件作为备份，应先停止服务或使用 SQLite 在线备份机制。

同一数据目录只允许一个进程使用。状态写入使用事务，脚本和进程变化依靠持久操作阶段恢复；断网不会触发反向停止。正在执行或待现场核对的操作会阻止同一 App 上的冲突变更。

“停止并解除阻塞”会停止该 App 所有已登记版本，再将初始化标记失效，保留数据和审计。无法确认进程停止时继续保持阻塞。版本数据不自动复制，清理操作不可恢复。仓库删除会先创建持久化清理任务，停止并删除客户端目标目录，包括离线节点上线后的清理。执行中或待核对的操作、运行中批次仍须先处理，删除期间禁止新部署。

单次脚本输出截断到 64 KiB，单条事件最多约 12 KiB，每个操作保留最多 2,000 条事件。launcher 将服务日志写入 `logs/agent.log`、`logs/server.log`，各保留当前和上一份，每份约 16 MiB。应用自行写入 `log/` 的文件须由应用轮转。

后台每小时维护：删除过期会话、验证挑战，清理 30 天前任务事件，压缩已结束 30 天的任务明细（保留去重记录），保留最近 10,000 条审计。待现场核对的操作不压缩。Agent 仅清理超过 7 天且无安装/任务引用的下载缓存、超过 24 小时的无活动临时目录，**不会自动清理任何版本的业务 data**。原始版本包由管理员显式管理。

控制端每天自动保存一致性 SQLite 快照、密钥及可用包归档，保留最近 7 份，位置为 `backups/YYYY-MM-DD/`。包归档采用同盘硬链接，避免每天重复占用整份包空间；这些本机备份不能抵御磁盘损坏，应由宿主机备份系统复制到其他介质。备份恢复只接受当前结构和一个不存在的新目录。恢复后的节点首次重连会核对较新的本地状态、隔离旧快照任务；在页面执行“停止并解除阻塞”后再部署，避免重放曾经执行过的清理或切换。快照之后尚未备份的仓库包需重新上传：

```bash
sudo /data/deployer/lifecycle/server/stop.sh
sudo /data/deployer/bin/deployerctl restore-backup \
  --backup /data/deployer/controller/backups/2026-10-18 \
  --data /data/deployer/controller-restored
# 修改 server.env 中的数据目录后启动
sudo /data/deployer/lifecycle/server/start.sh
```

若主机在快照之后接入，恢复后仍凭合法 mTLS 心跳重新登记，无需人工授权。保留原 Agent 数据目录和证书身份即可；恢复后同样核对较新的本地部署状态。

丢失密码和所有恢复码时，可在控制端本机停服后重置内置管理员。该命令同时解除验证码、撤销旧会话，并记录审计；运行用户应与控制端服务一致，避免生成错误属主的数据库文件：

```bash
sudo /data/deployer/lifecycle/server/stop.sh
sudo /data/deployer/bin/deployerctl reset-admin \
  --data /data/deployer/controller --confirm
sudo /data/deployer/lifecycle/server/start.sh
```

工具按长期运行设计，但客户端证书会自动续期；系统时间同步、CA 到期轮换、硬件和磁盘容量仍由宿主机保障。健康检查需能反映业务是否可用；无法确认进程已停止时保留阻塞，需要人工核对。

## 测试

```bash
npm --prefix web run build
go test -race ./...
go vet ./...
go test ./internal/supervisor -v -count=1
```

普通测试使用随机临时目录和随机本地 HTTP 端口，不依赖已有数据库。真实脚本测试使用随机临时目录，验证进程生命周期、重复启动、Agent 重新读取进程身份、残留子进程、超时取消、健康错误保存及卸载前停止；不依赖服务管理器。

设计与验收范围见 [DESIGN.md](DESIGN.md)，本轮可靠性复查见 [RELIABILITY.md](RELIABILITY.md)。当前数据库结构与 Agent 协议标记均为 `3`，仅维护这一版数据结构及协议，不提供开发期旧结构迁移。不要删除共享数据库或其他项目数据。

### 主机列表与部署选择

主机以详细表格展示，按 IP 稳定排序，支持每页 5/10/20/50/100 条、翻页和指定页跳转。勾选跨页保留；可全选、反选全部在线主机，或用表头选择当前页在线主机。每批最多 100 台。操作栏提供单机部署，仓库页仅负责应用和版本管理。

从主机列表发起部署时，控制端逐台核对目标版本；已部署同一 App、同一版本则记为跳过，不派发 Agent 任务、不改变修订号、启停状态或环境配置。批次显示处理数及跳过数。显式启动、重启和应用部署页的配置更新仍可使用。

删除仓库版本或应用前，控制台弹窗明确提示停止范围及永久删除配置、数据、日志。确认后客户端先停止进程并验证退出，再删除版本目录；删除应用或本机“解除部署”会删除整个应用目录。删除最后一个本地版本同时移除空应用目录，保留其他版本的数据。

主控记录“等待客户端清理”，收到所有目标客户端成功回报和不再包含目标的安装清单后，才删除仓库包及应用记录。离线客户端上线后继续，停止失败保留目录并显示原因，可在应用仓库重试删除；未完成删除的主机不能从主机列表移除。重复请求和客户端重启会继续已有流程，不会把未完成的清理显示为成功。操作记录与空部署修订保留用于审计、幂等和后续部署，页面不显示已清空的部署行。

这是显式破坏性删除，包含业务 data，无法通过撤回请求恢复。后台日常缓存维护仍不删除业务数据；原始包的共享下载缓存按缓存保留策略处理。新同步删除语义需要同时更新控制端和 Agent；旧 Agent 不支持的任务会报告失败，应先在“Agent 更新”升级客户端，再重试删除。
