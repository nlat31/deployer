# 公网正式发布与控制台验收（2026-10-19）

目标地址：18.138.124.233，SSH 别名 loom，Linux amd64。当前服务端发布为 Loom 0.8.16、Deployer 0.2.4；前文各节保留此前发布与清空操作的历史记录，最新现场结果见文末。

## 已通过的现场验收

使用真实公网控制台完成签名版本上传和两节点批量更新。本机 lenovo 与独立临时 root Agent 从 rc.3 更新至 rc.4；首台提交后暂停批次，第二台仍保持 rc.3；恢复后两台分别完成 60 秒试运行和 5 分钟稳定观察，批次 index 达到 2。

通过控制台上传签名有效但 exit 43 的候选程序，仅选择本机 Agent。候选版失败后自动恢复 rc.4，批次暂停，失败原因记录为 `trial exited: exit status 43`。真实 Loom Worker 的进程身份在更新及回退期间保持不变。

在两台客户端部署独立测试应用 console-cleanup-probe：

- 从控制台删除运行中的版本 2，进程退出、版本目录删除；版本 1 的数据仍存在。
- 从控制台删除整个应用，客户端先停止进程再删除整个应用目录。临时 Agent 暂停连接时，其目录仍保留、主控状态为 pending；重新连接后清理完成。
- 确认两个客户端应用目录均不存在，进程均退出，主控应用记录及安装清单均不再包含目标。

机器证据位于 `.cache/console-public-20261019/`：ui-report.json、delete-report.json、offline-report.json；控制台截图 live-agent-updates.png、delete-2.png、delete-app.png。验收脚本临时出现的 PATH、上传等待和重复打包时间戳问题已修正；上述通过结论来自修正后实际完成的流程。

## 正式发行包

两个项目的 dist 已在现场测试通过后清空并重新构建，无 rc 包残留。版本文件 VERSION、程序默认版本和前端元数据均调整为正式版本。

Loom dist 包含正式服务端、工具依赖包，以及独立/Deployer 托管两种方式各自的 slim/full Worker，共 6 个归档。Deployer dist 包含正式服务端和 Agent 两个归档，另附 Agent 程序及签名清单。所有归档附 SHA-256，共 8 个包，校验记录为 formal-packages.json。

完整 Worker 包使用从实际公网 Worker 缓存重建的公开签名组件，按原 manifest 列表复制，无客户端私钥。打包再次校验组件签名和文件摘要；没有替换公网签名密钥或绕过校验。slim 包首启从主控下载组件。

正式构建执行了 Loom 全量 Go、16 项组件、7 项发行和 10 项主控脚本测试；Deployer 发布工具、签名和 launcher 测试、正式 Agent 临时根目录幂等部署通过。此前全量 race 与浏览器测试记录见 RELIABILITY.md。

## 最终部署状态

正式服务端已替换并启动，Loom 26 条历史任务、8 条组件记录及 Deployer 原数据库文件均已核对保留。首次部署校验因重启后复用旧登录会话触发恢复旧程序；修正校验脚本重新登录后，正式替换成功，期间未恢复或重建数据库。最终以 `.cache/console-public-20261019/final-local.json`、formal-ui-report.json 和远端 `/data/releases/formal-20261019/deployment.json` 为准。正式替换使用 `/data/backups/formal-20261019` 保存一致性备份，保留数据库、配置、证书及历史业务记录，清除旧运行程序。

新版 Worker 的 App 标识及显示名称均为 loom-worker，版本 0.8.14，HTTP 健康检查为本机 18887/readyz、启动截止 300 秒。只上传仓库，等待用户部署，不自动下发新版本。


## 最终核验结果

- 公网 Loom 服务端 0.8.14、Deployer 服务端 0.2.0 均以 root 运行，HTTPS 就绪检查通过。
- 本机 Agent 已从控制台提交至正式 0.2.0，EUID 为 0，更新批次暂停。保留原有 16 台其他客户端记录，临时验收节点已删除，最终共 17 台节点。
- 本机旧 loom-worker 部署已通过停止并删除流程清理，`/data/deployer/agent/apps/` 无应用目录；临时 Agent 整个安装目录已删除，旧 Agent 运行版本只保留正式当前程序。
- App 标识及显示名称均为 loom-worker，正式 0.8.14 的 slim 托管包已入库为 available，SHA-256 为 `2ea8c31d1f09b0281167e2fe8e2fe4dce797a86ee8db644f7598d8b2ec1610e6`。没有节点部署或期望部署该版本，等待用户操作。
- 服务器旧运行版本及旧发布暂存目录已删除，保留数据库、配置和备份。备份恢复工具保存在 `/data/releases/formal-20261019/recovery.py`，数据库备份为 `/data/backups/formal-20261019`。

本次数据库压缩备份期间，控制端停机超过旧 Worker 的健康窗口，该 Worker 曾由既有健康恢复策略重启；这与此前 rc.4 批量更新/失败回退期间保持业务进程身份不变的验收是不同阶段。正式 Agent 更新时，运行中的 Worker 进程早于该 Agent 的试运行；最后已按旧版本清理要求停止并移除。

最终机器证据：final-local.json、final-public.json、formal-ui-report.json、remote-deployment.json、remote-cleanup.json、formal-packages.json。截图 formal-worker-ready.png 展示正式版本已入库；live-failure-reason.png 展示真实崩溃候选的控制台失败原因。

## 同版本控制端重打包与手动部署准备（后续操作）

按用户要求重新构建 Deployer 控制端 0.2.0，包改为直接包含 setup.sh、start.sh、stop.sh、remove.sh 和三个程序的平铺结构；版本号不变。SHA-256 为 `365d755887bc0e0a078040c814c6e7ea3a03b64f4a4ec5b02bf692904e179270`，覆盖服务器 `/data/releases/formal-20261019/deployer-server-0.2.0-linux-amd64.tar.gz`。

真实 root 隔离验证已通过：包成员和执行权限、脚本语法、版本号、重复 setup/start/stop/remove、重复启动保持原进程、HTTP 就绪及卸载保留数据。

当前状态已取代前述验收结束时的 Deployer 状态：服务器控制端已停止并移除旧安装程序，活动数据目录 `/data/deployer/controller` 已清空，原应用、发布、节点、用户及更新批次等数据不再保留于活动目录。证书及配置保留，历史备份未删除。根目录 `/data/deployer` 已放好新程序和脚本，但未执行 setup/start，留给用户手动部署。Loom 就绪检查仍通过，本机 Agent 未改动。

服务器执行：`cd /data/deployer`，然后 `sudo ./setup.sh`、`sudo ./start.sh`；停止执行 `sudo ./stop.sh`。现场证据为远端 `/data/releases/formal-20261019/manual-server-reset.json`，本地测试日志为 `.cache/console-public-20261019/repack-test.log`。

## 简化 App 生命周期后的发布与现场验收

最新实际运行版本为 Loom 服务端/Worker **0.8.16**、Deployer 服务端/Agent **0.2.3**。两个服务端已部署到 ssh loom 并以 root 启动；148.135.91.253、148.135.97.51 的签名 Agent 更新均已提交，两个受管 Worker 最终均为 root、HTTP 200、healthy=true、ready=true。最后一台 Agent 完成五分钟稳定观察后，更新批次已暂停。

本轮保留操作开始时的现有数据：26 条 Loom 任务、8 条组件记录，以及 Deployer 的两台节点、一项应用、原数据库文件、配置和证书。备份位于服务器 `/data/backups/manual-final-20261019`；launcher 修复前另有 `/data/backups/deployer-before-launcher-fix`。没有清空数据库。

职责边界保持简单：Deployer 执行管理员明确请求的部署、setup、启停和版本切换，反馈实际状态与错误。每次 start 都调用 App 的 start.sh，重复启动由 App 脚本自行处理；HTTP 2xx 即正常，不解析 ready、下载进度或“无进展”。App 失败不自动停止、重启或回退；Agent 重启也只核对 App 状态。Agent 自身签名更新仍有独立的试运行和失败回退。

真实公网验收已通过：

- 两台客户端 install/setup、重复 install，以及两节点批量部署。精简 Worker 冷下载期间返回 HTTP 200，部署成功，之后扫描/识别/采集组件全部就绪。
- 重复 stop/start；重复 start 确实进入 App 脚本并输出“工作端已运行”，原 PID 不变，私钥和版本数据保留。
- 管理员手动从 0.8.16 切到保留的 0.8.14，再切回 0.8.16；旧版本进程退出，版本私钥不变。
- 启动 exit 43 的临时坏版本后，目标版本保持失败，没有自动回退或重启；再通过真实控制台手动切回 0.8.16 成功。
- 故意损坏组件缓存，Worker 返回 HTTP 503 和 `component cache corrupted: adapter.lua`，后台收到相同错误正文；Worker 与 Agent PID 均未变化。恢复文件后，当前健康错误清除。
- SIGKILL Worker 后，后台报告“主进程未运行”，应用保持停止；重启 Agent 后仍保持停止。明确下发 start 才恢复就绪。
- 坏版本删除任务完成且客户端目录消失；两项临时坏版本均已删除，审计删除记录保留。临时组件故障、下载暂存文件及本地临时 SSH 密码文件已清理。

本轮发现并修复两项更新问题：旧 Agent 心跳携带已删除字段时无法领取更新；手动 setup 后遗留的旧更新请求可能再次切回旧 Agent。前者仅放宽同协议 Agent 心跳的可选字段解析，管理员操作仍严格校验；后者在安装和提交时清理已被替代的更新请求，并补充回归测试。

最终全量 `go test -race ./...`、go vet、前端生产构建和四项浏览器交互测试通过。最终 0.2.3 控制端发行包在随机临时目录完成真实 root 的四脚本重复执行测试，卸载保留数据；Loom 正式构建执行全量 Go、组件、发行及主控脚本测试。五项最终发行归档的 SHA-256 已重新核对。

证据位于 `.cache/manual-release-20261019/`：`acceptance.json` 包含 17 项现场检查与操作记录；`final-clients.json`、`server-final.json` 为最终进程及健康状态；`deployment-final.json` 为数据保留核对；`agent-update.json` 和 `agent-rollout-final.json` 为签名更新结果；`packages.json` 为最终包校验；`final-regression-after-fix.log`、`package-test-final.log` 为最终测试日志。截图为 `manual-switch.png`、`final-console.png`。删除检查按 `status=deleted` 和客户端目录确认，删除记录不会从审计清单消失。

本轮是实际公网功能与故障验收，不能替代长期浸泡测试或主机级高可用验证。

## 设备列表与心跳删除同步发布（2026-10-20）

Deployer 0.2.4 公网版已部署至 `ssh loom`（18.138.124.233），两台 Agent（148.135.91.253、148.135.97.51）通过签名批次升级到 0.2.4，完成试运行及稳定观察，均以 root 运行。原控制端数据库原地保留，原有 App/可用版本记录保留。两个现有 Loom Worker 0.8.16 的 PID、业务私钥摘要和健康状态保持不变。

- 设备列表必须先筛选 App 或“未部署”。未部署设备仅可部署；App 设备提供切换版本、停止、启动、日志、版本管理。分页选择跨页保留，改变筛选清空选择。独立“应用部署”页面移除。
- App 的 previous/previous_config、rollback/clear_rollback 操作及对应限制移除。管理员显式选择版本；不自动重启、停止或切回旧 App 版本。Agent 自身签名更新机制保留。
- 仓库删除立即移除记录和源包，不登记删除任务或待删除标记。心跳根据本地安装及实际目录与仓库的差异返回 `deleted`。客户端先停止并核验退出，再删除版本或整个 App 目录。失败时保留目录、通过 `cleanup_error` 反馈原因，后续心跳重试；离线设备重连后清理。
- 修复真实 stop.sh 错误被吞掉的问题：脚本失败返回退出码和错误输出，不再继续强制终止进程或删除目录。
- 心跳及设备版本管理包含部署时间和最近实际启动时间；旧进程没有采集到的启动时间仍显示未记录。

验证：完整 root `go test -race ./...`、`go vet ./...`、前端构建、11 项浏览器测试及最终服务端归档的四脚本幂等测试通过。公网使用临时 `device-cleanup-probe` 完成两机批量部署/setup、安装第二版本、真实控制台版本切换、启停、重复启动、日志、版本时间展示。运行中的版本删除时注入 stop.sh 错误，确认错误上报且目录保留；解除故障后心跳自动完成清理。整个 App 删除时暂停第二台 Agent，确认仓库立即消失、在线设备先停再删，离线设备重连后整个目录消失。两种仓库删除均未新增操作任务。临时 App、版本和两机目录均已清除。

发行包：`dist/deployer-server-0.2.4-linux-amd64.tar.gz`、`dist/deployer-agent-0.2.4-linux-amd64.tar.gz`；签名 Agent binary/manifest 同目录。远端归档位于 `/data/releases/device-console-20261019/`。控制端原数据备份位于 `/data/backups/deployer-before-device-console-0.2.4/`。

证据：[现场验收](.cache/device-console-20261019/acceptance.json)、[最终版本与数据核对](.cache/device-console-20261019/final.json)、[Go race](.cache/device-console-20261019/race-tests.log)、[浏览器测试](.cache/device-console-20261019/web-tests.log)、[跨页筛选测试](.cache/device-console-20261019/web-pagination-test.log)、[归档四脚本测试](.cache/device-console-20261019/package-test.log)。
